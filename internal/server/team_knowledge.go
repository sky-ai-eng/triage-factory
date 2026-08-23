package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/knowledgeevent"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

var knowledgeLog = logging.Component("teams/kb")

// --------------------------------------------------------------------
// /api/teams/{team_id}/kb — the team knowledge base.
//
// One KB per team, two roots (internal/kbstore). Visibility IS the root:
// `private/` is readable only through its own team's gate, `shared/` by any
// member of the org. So the same five routes serve both audiences, addressed
// at the team that OWNS the knowledge — an org member browsing another team's
// published notes calls these routes on THAT team id, and the roots they may
// read narrow to shared alone.
//
// Writes never cross teams: only a member with a writing role may upload,
// delete or move, and only within their own team's KB. That is the whole
// access model — there is no visibility column, no per-folder row, and
// publishing is the move verb rather than a flag.
// --------------------------------------------------------------------

const (
	// maxKnowledgeUploadBytes caps one multipart upload request. Knowledge is
	// prose a person wrote — notes, runbooks, diagrams — so this is generous
	// headroom rather than a target, and it bounds what a single request can
	// stream into the object store.
	maxKnowledgeUploadBytes = 32 << 20 // 32 MiB

	// maxKnowledgeUploadFiles caps how many files one upload request carries.
	// A folder drag-and-drop is the reason it is more than one; a directory
	// tree with thousands of files is the reason it is not unbounded.
	maxKnowledgeUploadFiles = 100
)

// knowledgeFileJSON is one KB entry on the wire. Root and path are separate
// fields rather than one joined string because they are separate inputs
// everywhere they are used again — the move body takes them apart, and the
// raw-read path joins them itself.
type knowledgeFileJSON struct {
	Root    string    `json:"root"`
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func knowledgeFileToJSON(f kbstore.FileInfo) knowledgeFileJSON {
	return knowledgeFileJSON{
		Root: string(f.Root),
		Path: f.Path,
		// The leaf name, so a client renders a row without re-splitting the
		// path and re-deriving the same answer three components deep.
		Name:    path.Base(f.Path),
		Size:    f.Size,
		ModTime: f.ModTime,
	}
}

// knowledgeListRequest is the body of POST /api/teams/{team_id}/kb/list.
//
// root narrows to one root; absent means every root the caller may read, which
// is what the page's combined tree renders. path_prefix narrows to a folder,
// matched by segment — "notes" selects notes/ and everything under it, never a
// sibling named notes-archive.
type knowledgeListRequest struct {
	Root       string `json:"root"`
	PathPrefix string `json:"path_prefix"`
	httpx.PageRequest
}

type knowledgeListFilters struct {
	Root       string `json:"root"`
	PathPrefix string `json:"path_prefix"`
}

// handleTeamKnowledgeList returns one page of the team's KB entries.
//
// The roots the caller may read are resolved from their membership, not from
// the request: a member sees both, any other org member sees shared alone. An
// explicit `root: "private"` from a non-member is not an error — it is a
// filter over a set they cannot see, so it answers an empty page, which is the
// same thing a team with no private knowledge answers and discloses nothing
// either way.
//
// Paging is applied over the resolved listing rather than pushed into the
// store: neither backend can page a prefix scan (an object store has no
// ordered cursor over a virtual tree, a filesystem no index), and a team KB is
// bounded by what people write rather than by row growth. The window is still
// honored on the wire so a client's loop is byte-identical to every other list.
//
// POST /api/teams/{team_id}/kb/list
func (s *Server) handleTeamKnowledgeList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := s.knowledgeTeamScope(w, r)
	if !ok {
		return
	}

	var req knowledgeListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.Root != "" {
		if _, err := kbstore.ParseRoot(req.Root); err != nil {
			v.Invalid("root", err.Error())
		}
	}
	if err := kbstore.ValidatePrefix(req.PathPrefix); err != nil {
		v.Invalid("path_prefix", err.Error())
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(knowledgeListFilters{
		Root:       req.Root,
		PathPrefix: req.PathPrefix,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	roots, ok := s.knowledgeReadableRoots(w, r, orgID, userID, teamID)
	if !ok {
		return
	}
	if req.Root != "" {
		roots = intersectRoots(roots, kbstore.Root(req.Root))
	}

	entries, err := s.knowledge().List(r.Context(), orgID, teamID, roots, req.PathPrefix)
	if err != nil {
		internalError(w, "teams/kb", err)
		return
	}

	total := len(entries)
	items := []knowledgeFileJSON{}
	if !page.CountOnly {
		for _, f := range windowEntries(entries, page.Offset, page.Limit) {
			items = append(items, knowledgeFileToJSON(f))
		}
	}
	httpx.WriteList(w, page, items, total)
}

// handleTeamKnowledgeRead streams one entry's bytes.
//
// The {path...} segment carries the root and the path together — the same
// "<root>/<path>" spelling a listing entry renders — so the address of a
// document is the address a person can copy out of the page.
//
// A path under a root the caller may not read answers 404, not 403: the entry
// is not in their listing either, so naming its existence would be the
// disclosure the two-root model exists to prevent.
//
// GET /api/teams/{team_id}/kb/{path...}
func (s *Server) handleTeamKnowledgeRead(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := s.knowledgeTeamScope(w, r)
	if !ok {
		return
	}
	ref, ok := knowledgeRefFromPath(w, r)
	if !ok {
		return
	}
	roots, ok := s.knowledgeReadableRoots(w, r, orgID, userID, teamID)
	if !ok {
		return
	}
	if !rootsContain(roots, ref.Root) {
		notFound(w, "knowledge file")
		return
	}

	// Stat first: it is what sizes the response, and it is also the one place
	// a folder path is refused before a backend that happens to have real
	// directories hands one back as a document.
	info, err := s.knowledge().Stat(r.Context(), orgID, teamID, ref)
	if err != nil {
		s.writeKnowledgeError(w, "teams/kb", err)
		return
	}
	body, err := s.knowledge().Open(r.Context(), orgID, teamID, ref)
	if err != nil {
		s.writeKnowledgeError(w, "teams/kb", err)
		return
	}
	defer func() { _ = body.Close() }()

	// Served as an opaque download rather than rendered: the bytes are
	// uploaded by org members and would otherwise be same-origin HTML the
	// browser executes. The filename is quoted through mime's own encoder so
	// a name with a space or a non-ASCII character survives the header.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": path.Base(ref.Path),
	}))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, body); err != nil && !httpx.IsClientGone(err) {
		knowledgeLog.Warn("stream knowledge file failed", "team", teamID, "ref", ref.String(), "error", err)
	}
}

// knowledgeUploadResultJSON reports what one uploaded file became. `outcome`
// distinguishes a new document from a replaced one so the page can say which
// happened; a client that does not care ignores the field.
type knowledgeUploadResultJSON struct {
	Root    string `json:"root"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Outcome string `json:"outcome"` // "created" | "replaced"
}

type knowledgeUploadResponse struct {
	Files []knowledgeUploadResultJSON `json:"files"`
}

// handleTeamKnowledgeUpload writes one or more files into the team's own KB.
//
// The request is multipart/form-data because the payload is file bytes: a
// `root` field naming which root to write into, an optional `path_prefix`
// naming the folder, and one or more `files` parts. Each part's own filename
// supplies the leaf name, and a part carrying a relative path (what a browser
// sends for a folder drop) keeps its shape under the prefix.
//
// A write is idempotent: uploading over an existing path replaces that
// document, which is what "save my updated runbook" means. The per-file
// outcome says which happened, so nothing is silently overwritten unreported.
//
// POST /api/teams/{team_id}/kb/files
func (s *Server) handleTeamKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := s.knowledgeTeamScope(w, r)
	if !ok {
		return
	}
	if !s.requireKnowledgeWriter(w, r, orgID, userID, teamID) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxKnowledgeUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "expected a multipart/form-data body carrying root, an optional path_prefix, and one or more files parts",
		})
		return
	}

	var (
		root       kbstore.Root
		rootSeen   bool
		pathPrefix string
		results    []knowledgeUploadResultJSON
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.writeKnowledgeUploadReadError(w, err)
			return
		}

		switch part.FormName() {
		case "root":
			value, ok := readKnowledgeFormValue(w, part, "root")
			if !ok {
				return
			}
			parsed, perr := kbstore.ParseRoot(value)
			if perr != nil {
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason: httpx.ReasonInvalidField, Message: perr.Error(), Field: "root",
				})
				return
			}
			root, rootSeen = parsed, true
		case "path_prefix":
			value, ok := readKnowledgeFormValue(w, part, "path_prefix")
			if !ok {
				return
			}
			if err := kbstore.ValidatePrefix(value); err != nil {
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "path_prefix",
				})
				return
			}
			pathPrefix = value
		case "files":
			// The fields the parts are written under must arrive first: the
			// body is streamed, so a root that turned up after a file part
			// would mean buffering the whole upload to find out where it goes.
			if !rootSeen {
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason:  httpx.ReasonInvalidField,
					Message: "the root field must appear before the files parts",
					Field:   "root",
				})
				return
			}
			if len(results) >= maxKnowledgeUploadFiles {
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason:  httpx.ReasonOutOfRange,
					Message: fmt.Sprintf("an upload carries at most %d files", maxKnowledgeUploadFiles),
					Field:   "files",
				})
				return
			}
			res, ok := s.storeUploadedKnowledge(w, r, orgID, teamID, root, pathPrefix, part)
			if !ok {
				return
			}
			results = append(results, res)
		default:
			// Strict decoding, the multipart spelling of it: an unknown field
			// is a caller that thinks this route accepts something it does not.
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: fmt.Sprintf("unknown form field %q", part.FormName()),
			})
			return
		}
		_ = part.Close()
	}

	if !rootSeen {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "root is required", Field: "root",
		})
		return
	}
	if len(results) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "at least one files part is required", Field: "files",
		})
		return
	}

	s.knowledgeNotifier().Publish(r.Context(), orgID, teamID, root)
	writeJSON(w, http.StatusOK, knowledgeUploadResponse{Files: results})
}

// handleTeamKnowledgeDelete removes the named path and everything beneath it.
//
// One route deletes a document and a folder alike, because in a filing system
// they are one intent — "this path should be gone" — and the alternative is a
// client-side walk that can stop halfway. The response reports how many
// entries went away so the page can say what it did.
//
// DELETE /api/teams/{team_id}/kb/{path...}
func (s *Server) handleTeamKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := s.knowledgeTeamScope(w, r)
	if !ok {
		return
	}
	ref, ok := knowledgeRefFromPath(w, r)
	if !ok {
		return
	}
	if !s.requireKnowledgeWriter(w, r, orgID, userID, teamID) {
		return
	}

	removed, err := s.knowledge().Delete(r.Context(), orgID, teamID, ref)
	if err != nil {
		s.writeKnowledgeError(w, "teams/kb", err)
		return
	}
	if removed == 0 {
		notFound(w, "knowledge file")
		return
	}
	s.knowledgeNotifier().Publish(r.Context(), orgID, teamID, ref.Root)
	writeJSON(w, http.StatusOK, knowledgeDeleteResponse{Removed: removed})
}

// knowledgeMoveRequest is the body of the move verb: where the subtree is now,
// and where it should be.
type knowledgeMoveRequest struct {
	FromRoot string `json:"from_root"`
	FromPath string `json:"from_path"`
	ToRoot   string `json:"to_root"`
	ToPath   string `json:"to_path"`
}

type knowledgeMoveResponse struct {
	// Moved is how many entries the transition touched — one for a document,
	// the subtree's size for a folder.
	Moved int `json:"moved"`
}

// knowledgeDeleteResponse is the delete verb's own shape. It reports `removed`
// rather than borrowing the move's `moved`: a client reading one field name
// across two different outcomes eventually renders one as the other.
type knowledgeDeleteResponse struct {
	// Removed is how many entries went away — one for a document, the
	// subtree's size for a folder.
	Removed int `json:"removed"`
}

// handleTeamKnowledgeMove relocates a document or a folder.
//
// This is the single verb behind publish (private→shared), unpublish
// (shared→private), rename and reorganize. They are one operation on the
// two-root layout — a path changes — and four routes would be four spellings
// of it. It is a verb route rather than a field write because a folder move is
// a multi-object transition the store has to plan as a unit: it refuses on a
// collision rather than overwriting, so a publish either lands whole or does
// not land.
//
// POST /api/teams/{team_id}/kb/move
func (s *Server) handleTeamKnowledgeMove(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := s.knowledgeTeamScope(w, r)
	if !ok {
		return
	}
	if !s.requireKnowledgeWriter(w, r, orgID, userID, teamID) {
		return
	}

	var req knowledgeMoveRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	from := kbstore.Ref{Root: parseKnowledgeRoot(&v, req.FromRoot, "from_root"), Path: req.FromPath}
	to := kbstore.Ref{Root: parseKnowledgeRoot(&v, req.ToRoot, "to_root"), Path: req.ToPath}
	validateKnowledgePathField(&v, req.FromPath, "from_path")
	validateKnowledgePathField(&v, req.ToPath, "to_path")
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	moved, err := s.knowledge().Move(r.Context(), orgID, teamID, from, to)
	if err != nil {
		s.writeKnowledgeError(w, "teams/kb", err)
		return
	}
	// Both roots are announced, each to its own audience: a publish empties
	// one listing and fills the other, and a page watching only the root it is
	// showing must hear about whichever one that is.
	s.knowledgeNotifier().Publish(r.Context(), orgID, teamID, from.Root, to.Root)
	writeJSON(w, http.StatusOK, knowledgeMoveResponse{Moved: moved})
}

// --- gates and shared plumbing ------------------------------------------

// knowledgeTeamScope resolves the org, the caller and the {team_id} segment,
// answering 404 for a team that is not in the caller's org. Every KB route
// starts here so the addressing grammar is written once.
func (s *Server) knowledgeTeamScope(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	orgID, ok = s.requireOrg(w, r)
	if !ok {
		return "", "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject
	teamID, ok = s.az.TeamIDFromPath(w, r, "teams/kb", orgID, userID)
	if !ok {
		return "", "", "", false
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	// One nil check for the whole surface, here rather than at each store call:
	// an unwired seam is a startup race or a wiring bug, and answering "not
	// configured" is the honest report. Silently listing nothing would read as
	// "this team has written no knowledge", which is a different fact.
	if s.teamKB == nil {
		httpx.WriteErrors(w, http.StatusServiceUnavailable, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "the knowledge store is not available on this server yet",
		})
		return "", "", "", false
	}
	return orgID, userID, teamID, true
}

// knowledgeReadableRoots resolves which of the team's roots this caller may
// read. Membership is the whole question: a member reads both roots, and any
// other member of the org reads the published one — which is what makes
// `shared/` org-wide without a second route or an ACL anywhere.
func (s *Server) knowledgeReadableRoots(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) ([]kbstore.Root, bool) {
	_, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "teams/kb", err)
		return nil, false
	}
	if role == "" {
		return []kbstore.Root{kbstore.RootShared}, true
	}
	return kbstore.Roots(), true
}

// requireKnowledgeWriter gates every write. Two refusals, and the difference
// matters: a caller who is not on the team gets 403 naming that (the team
// itself is org-visible, so there is nothing to hide by 404ing), and a member
// whose role is view-only gets the same view-only 403 every other team write
// answers with.
//
// Nothing here ever writes another team's KB: the team in the path is the team
// the caller must be a writing member of, full stop.
func (s *Server) requireKnowledgeWriter(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	_, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "teams/kb", err)
		return false
	}
	if role == "" {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason:  httpx.ReasonForbidden,
			Message: "a team's knowledge base is written by its own members",
		})
		return false
	}
	// An archived team is read-only, the same lifecycle boundary every other
	// team-scoped write observes.
	if !s.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return false
	}
	return s.az.RequireTeamWrite(w, r, orgID, userID, teamID)
}

// knowledgeRefFromPath parses the "<root>/<path…>" wildcard segment the
// per-file routes address a document by. A segment that is not a valid
// address is 404 rather than 400: it names nothing, and a 400 would confirm
// the shape of names that do exist.
func knowledgeRefFromPath(w http.ResponseWriter, r *http.Request) (kbstore.Ref, bool) {
	raw := strings.TrimPrefix(r.PathValue("path"), "/")
	rootPart, rest, found := strings.Cut(raw, "/")
	if !found {
		notFound(w, "knowledge file")
		return kbstore.Ref{}, false
	}
	root, err := kbstore.ParseRoot(rootPart)
	if err != nil {
		notFound(w, "knowledge file")
		return kbstore.Ref{}, false
	}
	if err := kbstore.ValidatePath(rest); err != nil {
		notFound(w, "knowledge file")
		return kbstore.Ref{}, false
	}
	return kbstore.Ref{Root: root, Path: rest}, true
}

func parseKnowledgeRoot(v *httpx.Validation, raw, field string) kbstore.Root {
	if raw == "" {
		v.Missing(field)
		return ""
	}
	root, err := kbstore.ParseRoot(raw)
	if err != nil {
		v.Invalid(field, err.Error())
		return ""
	}
	return root
}

func validateKnowledgePathField(v *httpx.Validation, raw, field string) {
	if raw == "" {
		v.Missing(field)
		return
	}
	if err := kbstore.ValidatePath(raw); err != nil {
		v.Invalid(field, err.Error())
	}
}

// writeKnowledgeError maps the store's sentinels onto the HTTP fault classes.
// An invalid address reaching here is a 400 with the store's own reason —
// every route validates first, so this is the backstop rather than the gate.
func (s *Server) writeKnowledgeError(w http.ResponseWriter, scope string, err error) {
	switch {
	case errors.Is(err, kbstore.ErrNotFound):
		notFound(w, "knowledge file")
	case errors.Is(err, kbstore.ErrExists):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: err.Error(),
		})
	case errors.Is(err, kbstore.ErrInvalidName):
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: err.Error(),
		})
	default:
		internalError(w, scope, err)
	}
}

// storeUploadedKnowledge streams one multipart part into the store and reports
// what it became. The part's own filename supplies the leaf; a browser sending
// a folder drop puts a relative path there, and that shape is preserved under
// the request's prefix so an uploaded directory arrives as a directory.
func (s *Server) storeUploadedKnowledge(w http.ResponseWriter, r *http.Request, orgID, teamID string, root kbstore.Root, pathPrefix string, part *multipart.Part) (knowledgeUploadResultJSON, bool) {
	rel := normalizeUploadFilename(uploadPartFilename(part))
	if rel == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "each files part must carry a filename", Field: "files",
		})
		return knowledgeUploadResultJSON{}, false
	}
	full := rel
	if pathPrefix != "" {
		full = pathPrefix + "/" + rel
	}
	if err := kbstore.ValidatePath(full); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "files",
		})
		return knowledgeUploadResultJSON{}, false
	}
	ref := kbstore.Ref{Root: root, Path: full}

	// Read before write, so the reply can say whether this replaced something.
	// It is a report, not a gate: a document overwritten between the two is
	// still overwritten, and the alternative — refusing — makes "save my
	// edited runbook" a delete-then-upload dance.
	outcome := "created"
	if _, err := s.knowledge().Stat(r.Context(), orgID, teamID, ref); err == nil {
		outcome = "replaced"
	} else if !errors.Is(err, kbstore.ErrNotFound) {
		internalError(w, "teams/kb", err)
		return knowledgeUploadResultJSON{}, false
	}

	counter := &countingReader{r: part}
	if err := s.knowledge().Put(r.Context(), orgID, teamID, ref, counter); err != nil {
		s.writeKnowledgeUploadReadError(w, err)
		return knowledgeUploadResultJSON{}, false
	}
	return knowledgeUploadResultJSON{
		Root:    string(ref.Root),
		Path:    ref.Path,
		Name:    path.Base(ref.Path),
		Size:    counter.n,
		Outcome: outcome,
	}, true
}

// writeKnowledgeUploadReadError renders a fault that surfaced while reading the
// request body. An over-cap body is the caller's fault and says so with the
// cap; anything else is ours.
func (s *Server) writeKnowledgeUploadReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpx.WriteErrors(w, http.StatusRequestEntityTooLarge, httpx.ErrorItem{
			Reason:  httpx.ReasonPayloadTooLarge,
			Message: fmt.Sprintf("an upload is at most %d bytes", maxKnowledgeUploadBytes),
			Field:   "files",
		})
		return
	}
	if errors.Is(err, kbstore.ErrInvalidName) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "files",
		})
		return
	}
	internalError(w, "teams/kb", err)
}

// readKnowledgeFormValue reads one non-file multipart field, bounded so a
// scalar field cannot be used to stream an unbounded body past the file cap.
func readKnowledgeFormValue(w http.ResponseWriter, part *multipart.Part, field string) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(part, kbstore.MaxPathLen+1))
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "could not read the " + field + " field", Field: field,
		})
		return "", false
	}
	if len(raw) > kbstore.MaxPathLen {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonOutOfRange,
			Message: fmt.Sprintf("%s is longer than %d bytes", field, kbstore.MaxPathLen),
			Field:   field,
		})
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

// uploadPartFilename reads a part's declared filename from its raw
// Content-Disposition rather than through Part.FileName, which bases the name
// and so flattens a folder upload into a pile of loose files. A browser
// uploading a directory puts the relative path there, and preserving it is how
// an uploaded folder arrives as a folder.
//
// Losing Base is not losing a guard: it only ever hid a bad name behind a
// plausible one (".." became the file "..", "/etc/x" became "x"), and the
// caller validates the result against the KB path rules, which refuse
// traversal, absolute and dot-prefixed segments outright.
func uploadPartFilename(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// normalizeUploadFilename turns a declared filename into a KB-relative path.
// Windows clients send backslashes; both spellings become the one
// slash-separated form the store validates. The result is validated by the
// caller — this only normalizes the separator and strips surrounding ones.
func normalizeUploadFilename(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	return strings.Trim(name, "/")
}

// countingReader counts the bytes streamed through it, so an upload reports
// the size it stored without a second Stat round trip.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// intersectRoots narrows a caller's readable roots to the one they asked for.
// A root they may not read yields an empty set rather than an error: it is a
// filter over knowledge they cannot see, and the empty page it produces is
// indistinguishable from a root with nothing in it — which is the point.
func intersectRoots(allowed []kbstore.Root, want kbstore.Root) []kbstore.Root {
	if rootsContain(allowed, want) {
		return []kbstore.Root{want}
	}
	return nil
}

func rootsContain(roots []kbstore.Root, want kbstore.Root) bool {
	for _, r := range roots {
		if r == want {
			return true
		}
	}
	return false
}

// windowEntries applies the resolved page window to a fully-materialized
// listing. An offset past the end is an empty page, matching what every
// store-backed list answers rather than erroring.
func windowEntries(entries []kbstore.FileInfo, offset, limit int) []kbstore.FileInfo {
	if offset >= len(entries) {
		return nil
	}
	end := len(entries)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return entries[offset:end]
}

// knowledge returns the process-wide KB seam. Non-nil by the time any route
// body reaches it — knowledgeTeamScope is every route's first call and refuses
// an unwired server there.
func (s *Server) knowledge() kbstore.KB { return s.teamKB }

func (s *Server) knowledgeNotifier() *knowledgeevent.Notifier { return s.teamKBNotifier }
