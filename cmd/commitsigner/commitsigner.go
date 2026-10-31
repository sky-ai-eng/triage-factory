// Package commitsigner is the CLI entrypoint for the `triagefactory
// commit-signer` subcommand, the deployment commit signer's operator surface:
//
//	triagefactory commit-signer status    show the signer and its health
//	triagefactory commit-signer verify    check it against GitHub now
//
// The deployment signer has no HTTP routes: its config is env on the control
// containers, so whoever can change or fix it has a shell there, and shell
// access to a control container is the trust boundary, the same as for
// `operator` and `instance`. Each answers for the signer the container
// it runs in is configured with (TF_GITHUB_COMMIT_SIGNER_*), and opens its own
// DB handle (db.OpenForCLI), independent of a running server.
package commitsigner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	signing "github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Handle dispatches the commit-signer subcommand.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	var run func(context.Context, io.Writer, *signing.DeploymentHealth) error
	switch args[0] {
	case "status":
		run = runStatus
	case "verify":
		run = runVerify
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown commit-signer subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "usage: triagefactory commit-signer %s\n", args[0])
		os.Exit(2)
	}
	health, closeFn := open()
	err := run(context.Background(), os.Stdout, health)
	_ = closeFn()
	if err != nil {
		fail("%s: %v", args[0], err)
	}
}

// open builds the deployment signer's health keeper the way a control pod
// does: the signer from this container's config, the stores over the admin
// DSN, and the GitHub credential resolver Verify re-reads a pushed commit
// with, which needs the secret key and the deployment App. Callers close the
// DB with closeFn.
func open() (health *signing.DeploymentHealth, closeFn func() error) {
	if runmode.Current() != runmode.ModeMulti {
		fail("local mode has no deployment commit signer: it signs commits only for organizations on a multi-mode deployment's GitHub App")
	}
	if runmode.Role() == runmode.RoleExecutor {
		fail("run it on a control container (TF_ROLE=control): an executor carries no deployment signer settings and no secret key")
	}
	signer, err := signing.DeploymentSignerFromEnv()
	if err != nil {
		fail("%v", err)
	}
	if !signer.Configured() {
		fail("this container has no deployment commit signer: set %s and %s on the control containers", signing.EnvDeploymentSignerLogin, signing.EnvDeploymentSignerKey)
	}
	key, err := aead.LoadKeyFromEnv(pgstore.EnvSecretEncryptionKey)
	if err != nil {
		fail("load secret encryption key: %v", err)
	}
	app, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		fail("read the deployment GitHub App from the environment: %v", err)
	}
	conn, _, err := db.OpenForCLI()
	if err != nil {
		fail("%v", err)
	}
	// TF_DATABASE_URL is the admin DSN, and every read and write here is a
	// system one, so it stands in for the app pool too.
	stores := pgstore.New(conn, conn, key)
	gh := ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil, ghclient.WithDeploymentApp(app))
	return signing.NewDeploymentHealth(noop.NewMeterProvider(), signer, stores, gh), conn.Close
}

// runStatus prints the signer as the last check left it. A key no control pod
// has registered yet is pending.
func runStatus(ctx context.Context, w io.Writer, h *signing.DeploymentHealth) error {
	row, err := h.Stores.DeploymentCommitSigners.GetSystem(ctx, h.Signer.Host, h.Signer.PublicKey)
	if err != nil {
		return err
	}
	if row == nil {
		row = &domain.DeploymentCommitSigner{Host: h.Signer.Host, PublicKey: h.Signer.PublicKey, Login: h.Signer.Login, Status: domain.CommitSignerPending}
	}
	printSigner(w, h.Signer, *row)
	if row.Status != domain.CommitSignerActive {
		fmt.Fprintln(w, "\nManaged organizations commit unsigned until it is active. Fix the account, then run `triagefactory commit-signer verify`.")
	}
	return nil
}

// runVerify checks the signer against GitHub now and prints it once it is
// active. A check that does not pass is an error naming what to fix.
func runVerify(ctx context.Context, w io.Writer, h *signing.DeploymentHealth) error {
	row, fault, err := h.Verify(ctx)
	if err != nil {
		return err
	}
	if fault != nil {
		return errors.New(fault.Message)
	}
	fmt.Fprintln(w, "The deployment commit signer is active.")
	fmt.Fprintln(w)
	printSigner(w, h.Signer, row)
	return nil
}

func printSigner(w io.Writer, signer *signing.DeploymentSigner, row domain.DeploymentCommitSigner) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	account := "not read yet"
	if row.GitHubUserID > 0 {
		account = strconv.FormatInt(row.GitHubUserID, 10)
	}
	fmt.Fprintf(tw, "login\t%s on %s (account %s)\n", row.Login, row.Host, account)
	if !strings.EqualFold(row.Login, signer.Login) {
		fmt.Fprintf(tw, "configured login\t%s\n", signer.Login)
	}
	status := string(row.Status)
	if row.StatusReason != "" {
		status += " (" + row.StatusReason + ")"
	}
	if !row.StatusSince.IsZero() {
		status += " since " + utc(row.StatusSince)
	}
	fmt.Fprintf(tw, "status\t%s\n", status)
	checked := "never"
	if row.CheckedAt != nil {
		checked = utc(*row.CheckedAt)
	}
	fmt.Fprintf(tw, "checked\t%s\n", checked)
	if row.GitHubUserID > 0 {
		fmt.Fprintf(tw, "commits as\t%s <%s>\n", row.CommitName(), signer.CommitEmail(row))
	}
	fmt.Fprintf(tw, "public key\t%s\n", row.PublicKey)
	_ = tw.Flush()
}

func utc(t time.Time) string {
	return t.UTC().Format(time.DateTime) + " UTC"
}

func printUsage() {
	fmt.Println(`triagefactory commit-signer — the deployment commit signer.

USAGE
  triagefactory commit-signer status    show the signer and its health
  triagefactory commit-signer verify    check it against GitHub now

NOTES
  The deployment commit signer is the account every organization on the
  deployment's GitHub App signs its agents' commits as, unless the
  organization registers its own (TF_GITHUB_COMMIT_SIGNER_*, on the control
  containers).

  verify looks for the configured key among the account's signing keys and,
  for a signer a pushed commit broke, re-reads that commit; when both pass, the
  signer is active. Only a signer a pushed commit broke needs it: a pending
  signer, or one broken because its key was missing, turns active at the next
  periodic check once the key is on the account, and verify does that now.

  Run it on a control container: it answers for the signer that container is
  configured with, and an executor carries none.`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory commit-signer: "+format+"\n", args...)
	os.Exit(1)
}
