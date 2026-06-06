// Debug scene for the 3D rewrite (SKY-196 / SKY-197).
//
// Section ported from the 2.5D factory: the new-commits intake,
// CI fan-out, CI Failed re-queue loopback, and stubs for the
// post-CI-Passed router and the not-yet-wired review-outcome
// splitter (placed for spatial planning).
//
//                                          .─────→ MC               ↑ top
//                                          ↑
//                                          | (pole_above)
//                                          |
//   merger ──→ NC ──→ S1 ──→ S2 ────────→──┼──→ CI Passed ──→ post-CI                  ⊕ review (unwired)
//   ▲                                      |
//   ▲                                      | (pole_below)
//   │                                      ↓
//   │                                      .─────→ CI Failed ──┐
//   │                                                          │
//   │       pole_3 ←────────── pole_2 ←────────── pole_1 ◀─────┘   (loopback)
//   │       │
//   └───────┘
//
// Items spawn at the merger's west input — appearing at its
// outside face — traverse the merger body, exit east into NC,
// and ride the main belt graph through S1/S2 to one of the CI
// stations. Stations the items must transit (NC, CI Passed, CI
// Failed) are wired passthrough (west.next = east.segment) so
// items don't dead-end at every recess. MC stays a true terminal.
//
// The CI Failed loopback is geometric only today: the simulator
// picks next[0] at every fork, so all items currently route to
// CI Passed → post-CI. Other branches (MC, CI Failed loop, post-CI
// outputs, review splitter) sit as static structure ready for the
// routing-decision layer that lands with real entity data.
//
// Path-offset math chains chevrons through stub → belt → stub at
// every junction, anchored at hardcoded station-stub UVs. Where a
// belt straddles two anchors (loopback to merger, CI Failed east
// face), the math accepts a small chevron jump at the upstream
// face — invisible at the splitter/router junctions but a tiny
// phase shift at the CI Failed east wall.

import { Vector3 } from '@babylonjs/core'

import { BELT_WORLD_SPEED } from './iso-belt'
import { CinematicDirector, type Shot } from './cinematic'
import { hashHue } from './iso-items'
import { placeEntity } from './place-entity'
import { DEFAULT_PORT_RECESS_DEPTH } from './iso-port'
import type { Pole } from './iso-pole'
import { IsoScene } from './iso-renderer'
import { buildRoutingTable, type RoutingTable } from './iso-routing'
import type { Router } from './iso-router'
import type { Station } from './iso-station'
import type { TransitPlacement } from './snapshot-chips'
import type { FactoryEntity, FactorySnapshot, FactoryStation } from '../types'

const FLOOR_CELL = 80
const FLOOR_SIZE = 4800 // 60 cells — extra room for downstream additions
const INITIAL_ZOOM_RADIUS = FLOOR_SIZE / 2

// ─── Cell-grid layout ─────────────────────────────────────────────
//
// Stations are 5×3 cells, splitters/mergers/poles are 1 cell.
// Vertical separation between station mid-rows is 4 cells (3-cell
// belt at 240 wu); horizontal spacing is mostly 1 cell (80 wu).
// The CI Failed loopback wraps below the main belts at row 5.

const STATION_W = 5
const STATION_D = 3
const STATION_H = 64

// Source side: PR Opened (above and slightly left of RFR), then
// Ready-for-Review, intake splitter, merger, then NC. PR Opened
// is a standard west-input/east-output station; four corner poles
// wrap the conveyor east → south → west → south → east, ducking
// under PR Opened along row 23 before dropping into RFR's west
// input at row 19.
const PR_OPENED_COL = 0
const PR_OPENED_ROW = 24 // mid-row 25
const PRO_POLE1_COL = 6 // 1-cell gap east of PR Opened east face
const PRO_POLE1_ROW = 25 // aligned with PR Opened east port mid-row
const PRO_POLE2_COL = 6
const PRO_POLE2_ROW = 22 // bottom-east corner; turns the chain west
const PRO_POLE3_COL = 2
const PRO_POLE3_ROW = 22 // bottom-west corner; turns south again
const PRO_POLE4_COL = 2
const PRO_POLE4_ROW = 19 // final corner; turns east into RFR.west
const READY_FOR_REVIEW_COL = 5
const READY_FOR_REVIEW_ROW = 18 // mid-row 19
const INTAKE_SPLITTER_COL = 11
const INTAKE_SPLITTER_ROW = 19
const MERGER_COL = 13
const MERGER_ROW = 19
const NC_COL = 15
const NC_ROW = 18 // mid-row 19

// Splitters and turn poles run along the source mid-row.
const S1_COL = 21
const S2_COL = 23
const SPLITTER_ROW = 19

// Destinations (5-col stations, west face at col 16). High-row =
// top of screen after the camera's α=+π/2 flip, so MC sits at
// high row to land at the top of the diagram and CI Failed at low
// row to land at the bottom.
const DEST_COL = 26
const MC_ROW = 22 // mid-row 23, top of screen
const CI_PASSED_ROW = 18 // mid-row 19
const CI_FAILED_ROW = 14 // mid-row 15, bottom of screen

// Turn poles between S2 and the top/bottom destinations.
const POLE_ABOVE_ROW = 23
const POLE_BELOW_ROW = 15

// Post-CI splitter sits 1 cell east of CI Passed. Then a 1-cell
// gap, the review-intake merger (loopback target for "changes
// requested" — south input unwired today), 1-cell gap, the
// Review Requested station, 1-cell gap, the review-outcomes
// splitter, and a 3-way fan to Review Commented (east) / Review
// Approved (top) / Changes Requested (bottom) — same shape as
// the CI fan.
const POST_CI_COL = 32
const POST_CI_ROW = 19
const RM_COL = 34 // review-intake merger
const RM_ROW = 19
const RR_COL = 36 // Review Requested station (5-wide)
const RR_ROW = 18 // mid-row 19
const REVIEW_COL = 42 // review-outcomes splitter
const REVIEW_ROW = 19

// Review-fan destination stations (5-col, west face at col 45).
// Mirror of the CI fan: top of screen = high row, bottom = low.
const REVIEW_DEST_COL = 45
const REVIEW_APPROVED_ROW = 22 // mid-row 23, top of screen
const REVIEW_COMMENTED_ROW = 18 // mid-row 19
const CHANGES_REQUESTED_ROW = 14 // mid-row 15, bottom of screen

// Turn poles between review splitter and the top/bottom review
// destinations.
const POLE_REVIEW_ABOVE_ROW = 23
const POLE_REVIEW_BELOW_ROW = 15

// CI Failed loopback corners. Pole 1 has been promoted to a
// merger — it consumes both the CI Failed east output AND the
// post-CI splitter's south output (sometimes people commit even
// when CI is passing, so the post-CI south branch routes back
// into the new-commits loop). The chain then runs straight south
// down col 22, wraps west along row 12, and climbs the merger
// column (col 3) back into the NC merger's south input.
//
// The loopback's horizontal stretch sits at row 12 — 2 rows below
// CI Failed (rows 14–16). Row 11 is freed up for the new
// "ready-for-review intake" chain (RFR_POLE_*) below.
const LOOP_MERGER_COL = 32 // column-aligned with post-CI splitter
const LOOP_MERGER_ROW = 15 // east of CI Failed, merger feeds south output
const LOOP_POLE2_COL = 32
const LOOP_POLE2_ROW = 12 // south-east corner, turns west
const LOOP_POLE3_COL = 13
const LOOP_POLE3_ROW = 12 // south-west corner, turns north

// Ready-for-Review intake corners. The intake splitter's south
// output drops to row 11; a 3-port splitter at the SW corner
// branches the chevron east toward RM (existing review-intake
// path) and south toward a new bottom_pole + bottom_merger that
// eventually routes to Closed.
const RFR_SPLITTER_COL = 11 // = INTAKE_SPLITTER_COL
const RFR_SPLITTER_ROW = 11
const RFR_POLE2_COL = 34 // = RM_COL
const RFR_POLE2_ROW = 11

// Bottom-row chain — south leg of the new RFR splitter. Drops
// straight south at col 11 to a turn pole, runs east at row 8,
// and merges with the bridge's south output at col 21. The
// merger's east output runs the full bottom of the layout to
// the Closed station at the far right.
const BOTTOM_POLE_COL = 11
const BOTTOM_POLE_ROW = 8
const BOTTOM_MERGER_COL = 21 // column-aligned with the bridge
const BOTTOM_MERGER_ROW = 8 // 1 cell south of bridge.south end

// Closed terminal station — flush with the right edge of the
// 60-cell floor (cols 55–59, east face at x=4800). Row 7 puts the
// mid-row at 8 so the west input port aligns with the chain at y=680.
const CLOSED_COL = 55
const CLOSED_ROW = 7 // mid-row 8 (= BOTTOM_MERGER_ROW)

// Changes-Requested splitter — 1 cell east of CR's east face. Sits
// at row 15 (= CR's mid-row) so its west port aligns with CR.east.
// South output drops to a new merger inserted on the
// bottom_merger → Closed belt; north output climbs to the new
// RC merger up at row 19 (joining the RC east-flowing chain);
// east output is reserved for a future destination.
const CR_SPLITTER_COL = 51 // = REVIEW_DEST_COL + STATION_W + 1
const CR_SPLITTER_ROW = 15

// Review Commented chain: RC.east flows through a 2-input merger
// (catching CR_SPLITTER's north output as well) and into a 3-port
// splitter east of it. The splitter fans north (up + around under
// the RA bridge via two corner poles, dead-ending for now) and
// south (drops to a new merger inserted on the row-8 Closed line).
const RC_MERGER_COL = 51 // column-aligned with CR_SPLITTER above
const RC_MERGER_ROW = 19 // = REVIEW_COMMENTED's mid-row
const RC_SPLITTER_COL = 53 // 1 cell east of RC_MERGER (with the 1-cell belt)
const RC_SPLITTER_ROW = 19

// Under-bridge route: two corner poles take the splitter's north
// output up one cell, west under the RA → Merged bridge, then
// turn UP at the second pole. A long vertical belt then climbs
// from the second pole straight through the bridge's z-shadow
// to a merger 5 cells above. Row 21 sits two cells south of the
// bridge's row-23 centerline so the belt has clearance under
// the elevated surface.
const BRIDGE_UNDER_POLE_1_COL = 53 // = RC_SPLITTER_COL
const BRIDGE_UNDER_POLE_1_ROW = 21
const BRIDGE_UNDER_POLE_2_COL = 51 // 2 cells west of pole 1
const BRIDGE_UNDER_POLE_2_ROW = 21

// Over-bridge merger: 5 cells north of pole_2, sitting at col 51
// (column-aligned with pole_2). The 4-cell vertical belt between
// them passes through the bridge's column at floor-z (~20 wu of
// clearance below the elevated surface). South input catches the
// pole_2 → merger climb; east input now wakes up to receive the
// "approved-but-needs-more-commits" branch off the RA → Merged
// path; west output exits left toward a future destination.
const OVER_BRIDGE_MERGER_COL = 51 // = BRIDGE_UNDER_POLE_2_COL
const OVER_BRIDGE_MERGER_ROW = 26 // = pole_2 row + 5

// RA → Merged splitter: sits on the flat connector right after
// the bridge. Even an approved review sometimes lands more commits
// before merge, so we fork that case off here. East output continues
// to Merged; north output drops into a turn pole that feeds the
// over-bridge merger (which will eventually route back into the
// new-commits chain).
const RA_SPLITTER_COL = 53 // bridge's east-edge column
const RA_SPLITTER_ROW = 23 // = REVIEW_APPROVED's mid-row

// Over-bridge pole: 1 cell east of OVER_BRIDGE_MERGER at the same
// row. Catches the splitter's south-flowing branch and turns it
// west into the merger.
const OVER_BRIDGE_POLE_COL = 53 // = RA_SPLITTER_COL
const OVER_BRIDGE_POLE_ROW = 26 // = OVER_BRIDGE_MERGER_ROW

// Merge Conflicts splitter: 2 cells east of MC's east face, at MC's
// mid-row. The 2-cell gap leaves room for clean wall renders on
// both ends (no double-recess collision). West input catches MC.east
// via the 2-cell belt; north output drops into the new
// MC_TAIL_MERGER on row 26 (which sits on the back-to-NC path);
// east and south outputs stay dead-end stubs reserved for the
// future MC → Closed connection.
const MC_SPLITTER_COL = 33 // 2 cells east of MC's east face
const MC_SPLITTER_ROW = 23 // = MC's mid-row

// MC tail merger: same row as OVER_BRIDGE_MERGER (row 26), at
// col 33 directly above MC_SPLITTER. South input catches the
// splitter's vertical north output; east input now wakes up to
// receive OVER_BRIDGE_MERGER's west output (the merged
// CR/RC/RA stream); west output is dormant — it'll continue
// west on the path back to New Commits.
const MC_TAIL_MERGER_COL = 33 // = MC_SPLITTER_COL
const MC_TAIL_MERGER_ROW = 26 // = OVER_BRIDGE_MERGER_ROW

// MC foot merger: lands on the row-8 Closed line at col 33,
// inserted between bottom_merger and CR_MERGER. North input
// catches the south branch of MC_SPLITTER (which descends through
// two bridges over the POST_CI→RM and RFR-intake crossings).
const MC_FOOT_MERGER_COL = 33 // = MC_SPLITTER_COL
const MC_FOOT_MERGER_ROW = 8 // = BOTTOM_MERGER_ROW

// Back-to-NC pole: 20 cells west of MC_FOOT_MERGER at the row-26
// elevation, column-aligned with the NC intake merger so its
// south output drops straight into MERGER.north. Catches the
// row-26 west-flowing return path (MC_TAIL_MERGER ← reviews
// section) and elbows it south.
const BACK_TO_NC_POLE_COL = 13 // = MERGER_COL
const BACK_TO_NC_POLE_ROW = 26 // = MC_TAIL_MERGER_ROW

// NC loop merger: inserted on the row-26 west-bound belt directly
// above S1, capturing S1's new north output. Closes the
// "NC commits beget more NC commits" short loop — items can leave
// S1 going north, fold into the back-to-NC stream, and re-enter
// the NC merger via the existing 6-cell drop at col 13.
const NC_LOOP_MERGER_COL = 21 // = S1_COL
const NC_LOOP_MERGER_ROW = 26 // = MC_TAIL_MERGER_ROW

// Two south-flowing bridges in the col-33 descent. Each is 3 cells
// (1 ramp + 1 flat + 1 ramp) with the flat peak directly over its
// crossing belt. Bridge 1 covers the POST_CI→RM belt at row 19;
// bridge 2 covers the RFR-intake belt at row 11.
const MC_BRIDGE_1_NORTH_ROW = 20 // peak at row 19
const MC_BRIDGE_2_NORTH_ROW = 12 // peak at row 11
const MC_BRIDGE_CELL_COUNT = 3

// RC tail merger: inserted on the bottom_merger → Closed belt at
// col 53 (between CR_MERGER and Closed). Catches the splitter's
// south output (10-cell vertical drop) and merges into the
// Closed-bound flow.
const RC_TAIL_MERGER_COL = 53 // = RC_SPLITTER_COL
const RC_TAIL_MERGER_ROW = 8 // = CR_MERGER_ROW (= BOTTOM_MERGER_ROW)

// CR merger — sits on the row-8 belt between bottom_merger and
// Closed. Catches the CR splitter's south leg via a 6-cell vertical
// drop at col 51, and merges it into the eastward flow toward the
// Closed station.
const CR_MERGER_COL = 51 // column-aligned with CR_SPLITTER
const CR_MERGER_ROW = 8 // = BOTTOM_MERGER_ROW

// Merged terminal station — flush with the right edge, vertically
// aligned with Closed (same col 55). Row 22 mirrors Review Approved
// so a horizontal bridge connects them at the same belt height.
const MERGED_COL = 55 // = CLOSED_COL
const MERGED_ROW = 22 // = REVIEW_APPROVED_ROW

// RA → Merged bridge: 3 cells east-west at row 22 (mid-row 23,
// y=1880). 1 ramp up + 1 flat peak + 1 ramp down. The remaining
// 2 cells to Merged.west are filled with a normal flat belt so
// the bridge is compact and the chip rolls onto Merged at floor
// level. No belts cross underneath; elevation reads as a "special
// arrival" lane rather than a traffic separator.
const RA_BRIDGE_CELL_COUNT = 3

// Bridge — vertical north→south overpass at col 21 (column-aligned
// with S1, the splitter just downstream of NC). Fed from S1.south
// via a straight connector belt; crosses both south belts at once
// (RFR intake at row 11, CI Failed loopback at row 12). 6 cells
// total (1 ramp up + 4 flat peak + 1 ramp down) puts the flat span
// over rows 10–13 with a 1-cell margin around each crossing. The
// bridge's south end is a dead-end today; items entering get
// despawned. S1's west fan still picks east at next[0], so the
// bridge sits dormant until a routing decision flips items down it.
const BRIDGE_COL = 21 // = S1_COL
const BRIDGE_NORTH_ROW = 14 // northmost cell in the bridge span
const BRIDGE_CELL_COUNT = 6 // 1 ramp up + 4 flat peak + 1 ramp down
// Apex floor-z. Item top on a lower belt sits at z ≈ 22.5
// (CONVEYOR_HEIGHT + ITEM_LIFT + ITEM_HEIGHT). The bridge surface
// sits at peak + CONVEYOR_HEIGHT = 36, leaving ~13.5 wu of visible
// clearance — enough to read as "above" without looking stilted.
const BRIDGE_PEAK_HEIGHT = 28

// ─── Path-offset math ─────────────────────────────────────────────

const SPACING = 54 // CHEVRON_SPACING_WORLD
const ROUTER_STUB_LEN = 12 // ROUTER_RECESS_DEPTH (14) − STUB_BACK_GAP (2)
// Station east output stub: UV at wall plane = (60 − 2) mod 54 = 4.
const STATION_EAST_UV_AT_WALL = 4
const TURN_ARC_LENGTH = (Math.PI * FLOOR_CELL) / 4

// Belt lengths derive from the cell-grid layout. The RFR → intake
// splitter belt anchors forward off RFR's hardcoded east stub UV,
// so the belt length doesn't enter the chevron math.
const BELT_PRO_TO_POLE1_LEN = (PRO_POLE1_COL - (PR_OPENED_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_POLE1_TO_POLE2_LEN = (PRO_POLE1_ROW - (PRO_POLE2_ROW + 1)) * FLOOR_CELL // 240
const BELT_POLE2_TO_POLE3_LEN = (PRO_POLE2_COL - (PRO_POLE3_COL + 1)) * FLOOR_CELL // 400
const BELT_POLE3_TO_POLE4_LEN = (PRO_POLE3_ROW - (PRO_POLE4_ROW + 1)) * FLOOR_CELL // 240
// pole_4 → RFR.west belt's length doesn't enter the chevron math
// (chain anchors forward from PR Opened.east; jump lands at RFR
// west wall), so the length is implicit in the belt's geometry.
const BELT_INTAKE_SPLITTER_TO_MERGER_LEN = (MERGER_COL - (INTAKE_SPLITTER_COL + 1)) * FLOOR_CELL // 80
const BELT_MERGER_TO_NC_LEN = (NC_COL - (MERGER_COL + 1)) * FLOOR_CELL // 80
const BELT_NC_TO_S1_LEN = (S1_COL - (NC_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_S1_TO_S2_LEN = (S2_COL - (S1_COL + 1)) * FLOOR_CELL // 80
const BELT_S2_TO_CI_PASSED_LEN = (DEST_COL - (S2_COL + 1)) * FLOOR_CELL // 160
const BELT_S2_TO_POLE_LEN = (POLE_ABOVE_ROW - (SPLITTER_ROW + 1)) * FLOOR_CELL // 240
const BELT_POLE_TO_DEST_LEN = (DEST_COL - (S2_COL + 1)) * FLOOR_CELL // 160
const BELT_CI_PASSED_TO_POST_CI_LEN = (POST_CI_COL - (DEST_COL + STATION_W)) * FLOOR_CELL // 80
// Review chain belts. The merger→RR belt's length doesn't enter
// the offset math (chain anchors forward off the merger east face;
// any chevron mismatch lands at RR's hardcoded wall UV), so it's
// derived implicitly by addBelt from port positions.
const BELT_POSTCI_TO_RM_LEN = (RM_COL - (POST_CI_COL + 1)) * FLOOR_CELL // 80
const BELT_RR_TO_REVIEW_LEN = (REVIEW_COL - (RR_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_REVIEW_TO_DEST_LEN = (REVIEW_DEST_COL - (REVIEW_COL + 1)) * FLOOR_CELL // 160
const BELT_REVIEW_TO_REVIEW_POLE_LEN = (POLE_REVIEW_ABOVE_ROW - (REVIEW_ROW + 1)) * FLOOR_CELL // 240
const BELT_REVIEW_POLE_TO_DEST_LEN = BELT_REVIEW_TO_DEST_LEN // 160
// Loopback belts. Loop pole 1 is now a merger so the belt names
// reflect that (CIF → loop_merger.west, post-CI south → loop_merger.north).
const BELT_CIF_TO_LOOP_MERGER_LEN = (LOOP_MERGER_COL - (DEST_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_POSTCI_SOUTH_TO_LOOP_MERGER_LEN = (POST_CI_ROW - (LOOP_MERGER_ROW + 1)) * FLOOR_CELL // 240
const BELT_LOOP_MERGER_TO_LOOP2_LEN = (LOOP_MERGER_ROW - (LOOP_POLE2_ROW + 1)) * FLOOR_CELL // 160
const BELT_LOOP2_TO_LOOP3_LEN = (LOOP_POLE2_COL - (LOOP_POLE3_COL + 1)) * FLOOR_CELL // 1440
const BELT_LOOP3_TO_MERGER_LEN = (MERGER_ROW - (LOOP_POLE3_ROW + 1)) * FLOOR_CELL // 480

// Ready-for-Review intake belts. The chain hangs off the intake
// splitter's south output, runs through the RFR splitter (which
// also branches a south leg toward the bottom merger), and climbs
// into the review-intake merger's south input.
const BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_SPLITTER_LEN =
  (INTAKE_SPLITTER_ROW - (RFR_SPLITTER_ROW + 1)) * FLOOR_CELL // 560
const BELT_RFR_SPLITTER_TO_RFR_POLE2_LEN = (RFR_POLE2_COL - (RFR_SPLITTER_COL + 1)) * FLOOR_CELL // 1760
const BELT_RFR_POLE2_TO_RM_LEN = (RM_ROW - (RFR_POLE2_ROW + 1)) * FLOOR_CELL // 560

// Bottom chain belts: RFR splitter.south → bottom_pole.north →
// bottom_merger.west. The bridge feeds bottom_merger.north
// directly (zero-length connector — bridge.south end and the
// merger's north wall meet at the same point). bottom_merger.east
// runs east to CR_MERGER, then to RC_TAIL_MERGER (catching the
// new RC_SPLITTER south chain), then on to Closed at the far right.
const BELT_RFR_SPLITTER_SOUTH_TO_BOTTOM_POLE_LEN =
  (RFR_SPLITTER_ROW - (BOTTOM_POLE_ROW + 1)) * FLOOR_CELL // 160
const BELT_BOTTOM_MERGER_TO_MC_FOOT_MERGER_LEN =
  (MC_FOOT_MERGER_COL - (BOTTOM_MERGER_COL + 1)) * FLOOR_CELL // 880
const BELT_MC_FOOT_MERGER_TO_CR_MERGER_LEN = (CR_MERGER_COL - (MC_FOOT_MERGER_COL + 1)) * FLOOR_CELL // 1360
const BELT_CR_MERGER_TO_RC_TAIL_MERGER_LEN = (RC_TAIL_MERGER_COL - (CR_MERGER_COL + 1)) * FLOOR_CELL // 80
const BELT_RC_TAIL_MERGER_TO_CLOSED_LEN = (CLOSED_COL - (RC_TAIL_MERGER_COL + 1)) * FLOOR_CELL // 80

// CR splitter chain: CR.east → 1-cell belt → CR_SPLITTER.west,
// then CR_SPLITTER.south drops 6 cells south to CR_MERGER.north.
// The south leg is forward-anchored off CR_SPLITTER.south's wall
// UV; the small chevron jump lands at CR_MERGER.north, so the
// drop's length doesn't enter the chevron math. Same forward-anchor
// for the new north leg climbing into RC_MERGER's south input.
const BELT_CR_TO_CR_SPLITTER_LEN = (CR_SPLITTER_COL - (REVIEW_DEST_COL + STATION_W)) * FLOOR_CELL // 80

// RC chain: RC.east → 1-cell belt → RC_MERGER → 1-cell belt →
// RC_SPLITTER. Splitter then fans north (under-bridge route) and
// south (drop to RC_TAIL_MERGER on the Closed line).
//
// Forward-anchor from RC.east stub UV (4); chevron jumps land at
// RC_MERGER.south wall (where the CR-splitter north chain meets it)
// and at the splitter's downstream port walls.
const BELT_RC_TO_RC_MERGER_LEN = (RC_MERGER_COL - (REVIEW_DEST_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_RC_MERGER_TO_RC_SPLITTER_LEN = (RC_SPLITTER_COL - (RC_MERGER_COL + 1)) * FLOOR_CELL // 80
const BELT_RC_SPLITTER_NORTH_TO_UNDER_POLE_1_LEN =
  (BRIDGE_UNDER_POLE_1_ROW - (RC_SPLITTER_ROW + 1)) * FLOOR_CELL // 160
const BELT_UNDER_POLE_1_TO_UNDER_POLE_2_LEN =
  (BRIDGE_UNDER_POLE_1_COL - (BRIDGE_UNDER_POLE_2_COL + 1)) * FLOOR_CELL // 160
// RC_SPLITTER.south → RC_TAIL_MERGER.north length doesn't enter
// the chevron math (forward-anchored off the splitter's south
// wall; jump lands at the merger's north port frame).

const mod = (x: number) => ((x % SPACING) + SPACING) % SPACING

// Source: RFR.east → intake_splitter → merger → NC.west. Anchor
// at NC.west wall (UV 0) and walk backward through merger body,
// belt to splitter, splitter body, belt to RFR. The chevron jump
// (if any) lands at RFR.east, where the station's hardcoded UV
// (= STATION_EAST_UV_AT_WALL) meets the upstream belt.
const BELT_MERGER_TO_NC = mod(-BELT_MERGER_TO_NC_LEN)
const MERGER_EAST_OFFSET = mod(BELT_MERGER_TO_NC - ROUTER_STUB_LEN)
const MERGER_WEST_OFFSET = mod(MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // body-continuous
const MERGER_SOUTH_OFFSET = mod(MERGER_EAST_OFFSET - ROUTER_STUB_LEN)
const MERGER_NORTH_OFFSET = MERGER_WEST_OFFSET // body-continuous (input wall UV)
const BELT_INTAKE_SPLITTER_TO_MERGER = mod(MERGER_WEST_OFFSET - BELT_INTAKE_SPLITTER_TO_MERGER_LEN)
const INTAKE_SPLITTER_EAST_OFFSET = mod(BELT_INTAKE_SPLITTER_TO_MERGER - ROUTER_STUB_LEN)
const INTAKE_SPLITTER_WEST_OFFSET = mod(INTAKE_SPLITTER_EAST_OFFSET - ROUTER_STUB_LEN)
// RFR.east → intake_splitter.west: belt anchored forward off the
// RFR east stub (UV 4); chevron jump (if any) lands at the
// splitter's west wall.
const BELT_RFR_TO_INTAKE_SPLITTER = STATION_EAST_UV_AT_WALL

// PR Opened.east → 4 corner poles → RFR.west. Anchored forward
// off PR Opened's east stub (wall UV = STATION_EAST_UV_AT_WALL).
// Each pole adds TURN_ARC_LENGTH to the chevron offset; the small
// residual chevron jump lands at RFR.west.
const BELT_PRO_TO_POLE1 = STATION_EAST_UV_AT_WALL
const PRO_POLE1_OFFSET = mod(BELT_PRO_TO_POLE1 + BELT_PRO_TO_POLE1_LEN)
const BELT_POLE1_TO_POLE2 = mod(PRO_POLE1_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE2_OFFSET = mod(BELT_POLE1_TO_POLE2 + BELT_POLE1_TO_POLE2_LEN)
const BELT_POLE2_TO_POLE3 = mod(PRO_POLE2_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE3_OFFSET = mod(BELT_POLE2_TO_POLE3 + BELT_POLE2_TO_POLE3_LEN)
const BELT_POLE3_TO_POLE4 = mod(PRO_POLE3_OFFSET + TURN_ARC_LENGTH)
const PRO_POLE4_OFFSET = mod(BELT_POLE3_TO_POLE4 + BELT_POLE3_TO_POLE4_LEN)
const BELT_POLE4_TO_RFR = mod(PRO_POLE4_OFFSET + TURN_ARC_LENGTH)

// NC.east → S1 → S2: chain forward continuous from NC east stub.
const BELT_NC_TO_S1 = STATION_EAST_UV_AT_WALL
const S1_WEST_OFFSET = mod(BELT_NC_TO_S1 + BELT_NC_TO_S1_LEN)
const S1_EAST_OFFSET = mod(S1_WEST_OFFSET + ROUTER_STUB_LEN) // body-continuous
const S1_SOUTH_OFFSET = S1_EAST_OFFSET // body-continuous (bridge feed)
const S1_NORTH_OFFSET = S1_EAST_OFFSET // body-continuous (NC loop feed)
const BELT_S1_TO_S2 = mod(S1_EAST_OFFSET + ROUTER_STUB_LEN)
const S2_WEST_OFFSET = mod(BELT_S1_TO_S2 + BELT_S1_TO_S2_LEN)

// S1.south → straight connector belt → bridge north entry. The
// connector pathOffset comes off S1's south wall UV; the bridge
// picks up where the connector ends. Chevrons stay continuous from
// S1's body, through the connector, into the ramp-up, across the
// flat peak, and down the south ramp — buildCurvedBelt computes
// 3D arc length so the ramps don't break the tile.
const BELT_S1_SOUTH_TO_BRIDGE_LEN = (SPLITTER_ROW - (BRIDGE_NORTH_ROW + 1)) * FLOOR_CELL // 320
const BELT_S1_SOUTH_TO_BRIDGE = mod(S1_SOUTH_OFFSET + ROUTER_STUB_LEN)
const BRIDGE_PATH_OFFSET = mod(BELT_S1_SOUTH_TO_BRIDGE + BELT_S1_SOUTH_TO_BRIDGE_LEN)

// East destination: anchor at CI Passed (UV 0); S2.east adopts.
const BELT_S2_TO_CI_PASSED = mod(-BELT_S2_TO_CI_PASSED_LEN)
const S2_EAST_OFFSET = mod(BELT_S2_TO_CI_PASSED - ROUTER_STUB_LEN)

// Post-CI splitter: belt continuous forward from CI Passed.east
// (UV 4); post-CI's outputs adopt body-continuous values for
// later belt extensions.
const BELT_CI_PASSED_TO_POST_CI = STATION_EAST_UV_AT_WALL
const POST_CI_WEST_OFFSET = mod(BELT_CI_PASSED_TO_POST_CI + BELT_CI_PASSED_TO_POST_CI_LEN)
const POST_CI_OUTPUT_OFFSET = mod(POST_CI_WEST_OFFSET + ROUTER_STUB_LEN)

// Forward chain past post-CI: post-CI.east → review-intake merger
// → Review Requested → review-outcomes splitter. Body-continuous
// from post-CI east (UV at wall = POST_CI_OUTPUT_OFFSET + 12 = 0).
const BELT_POSTCI_TO_RM = mod(POST_CI_OUTPUT_OFFSET + ROUTER_STUB_LEN)
const RM_WEST_OFFSET = mod(BELT_POSTCI_TO_RM + BELT_POSTCI_TO_RM_LEN)
const RM_EAST_OFFSET = mod(RM_WEST_OFFSET + ROUTER_STUB_LEN) // body-continuous
// South input: loopback target, unwired today; body-continuous
// with the rest of the merger.
const RM_SOUTH_OFFSET = RM_WEST_OFFSET
const BELT_RM_TO_RR = mod(RM_EAST_OFFSET + ROUTER_STUB_LEN) // RM east wall UV

// RR east → review splitter west: anchor at RR east stub (UV 4).
const BELT_RR_TO_REVIEW = STATION_EAST_UV_AT_WALL
const REVIEW_WEST_OFFSET = mod(BELT_RR_TO_REVIEW + BELT_RR_TO_REVIEW_LEN)

// Review splitter outputs — each computed backward from its
// destination station's west wall (UV 0), so chevrons land
// continuous at the destination (the user-visible station face)
// at the cost of small phase shifts at the splitter face.
const BELT_REVIEW_TO_REVIEW_COMMENTED = mod(-BELT_REVIEW_TO_DEST_LEN)
const REVIEW_EAST_OFFSET = mod(BELT_REVIEW_TO_REVIEW_COMMENTED - ROUTER_STUB_LEN)

const BELT_POLE_REVIEW_ABOVE_TO_RA = mod(-BELT_REVIEW_POLE_TO_DEST_LEN)
const POLE_REVIEW_ABOVE_OFFSET = mod(BELT_POLE_REVIEW_ABOVE_TO_RA - TURN_ARC_LENGTH)
const BELT_REVIEW_TO_POLE_REVIEW_ABOVE = mod(
  POLE_REVIEW_ABOVE_OFFSET - BELT_REVIEW_TO_REVIEW_POLE_LEN,
)
const REVIEW_NORTH_OFFSET = mod(BELT_REVIEW_TO_POLE_REVIEW_ABOVE - ROUTER_STUB_LEN)

const BELT_POLE_REVIEW_BELOW_TO_CR = mod(-BELT_REVIEW_POLE_TO_DEST_LEN)
const POLE_REVIEW_BELOW_OFFSET = mod(BELT_POLE_REVIEW_BELOW_TO_CR - TURN_ARC_LENGTH)
const BELT_REVIEW_TO_POLE_REVIEW_BELOW = mod(
  POLE_REVIEW_BELOW_OFFSET - BELT_REVIEW_TO_REVIEW_POLE_LEN,
)
const REVIEW_SOUTH_OFFSET = mod(BELT_REVIEW_TO_POLE_REVIEW_BELOW - ROUTER_STUB_LEN)

// Above-S2 chain (toward MC): pole sits at high-row, fed by S2.NORTH.
const BELT_POLE_ABOVE_TO_MC = mod(-BELT_POLE_TO_DEST_LEN)
const POLE_ABOVE_OFFSET = mod(BELT_POLE_ABOVE_TO_MC - TURN_ARC_LENGTH)
const BELT_S2_TO_POLE_ABOVE = mod(POLE_ABOVE_OFFSET - BELT_S2_TO_POLE_LEN)
const S2_NORTH_OFFSET = mod(BELT_S2_TO_POLE_ABOVE - ROUTER_STUB_LEN)

// Below-S2 chain (toward CI Failed): mirror — pole at low-row,
// fed by S2.SOUTH.
const BELT_POLE_BELOW_TO_CI_FAILED = mod(-BELT_POLE_TO_DEST_LEN)
const POLE_BELOW_OFFSET = mod(BELT_POLE_BELOW_TO_CI_FAILED - TURN_ARC_LENGTH)
const BELT_S2_TO_POLE_BELOW = mod(POLE_BELOW_OFFSET - BELT_S2_TO_POLE_LEN)
const S2_SOUTH_OFFSET = mod(BELT_S2_TO_POLE_BELOW - ROUTER_STUB_LEN)

// CI Failed → loopback → NC merger.south: anchor at NC merger's
// south wall and walk backward through three belts, two corner
// poles, and a 3-port loop merger. Chevron jumps land at CI
// Failed's east wall and at post-CI's south wall (the two
// hardcoded upstream stubs we can't tune individually).
const BELT_LOOP3_TO_MERGER = mod(MERGER_SOUTH_OFFSET - BELT_LOOP3_TO_MERGER_LEN)
const LOOP_POLE3_OFFSET = mod(BELT_LOOP3_TO_MERGER - TURN_ARC_LENGTH)
const BELT_LOOP2_TO_LOOP3 = mod(LOOP_POLE3_OFFSET - BELT_LOOP2_TO_LOOP3_LEN)
const LOOP_POLE2_OFFSET = mod(BELT_LOOP2_TO_LOOP3 - TURN_ARC_LENGTH)
const BELT_LOOP_MERGER_TO_LOOP2 = mod(LOOP_POLE2_OFFSET - BELT_LOOP_MERGER_TO_LOOP2_LEN)
// Loop merger: south is the output (chevron-continuous with the
// belt heading downstream); west and north are inputs, both
// body-continuous via the recess back.
const LOOP_MERGER_SOUTH_OFFSET = mod(BELT_LOOP_MERGER_TO_LOOP2 - ROUTER_STUB_LEN)
const LOOP_MERGER_WEST_OFFSET = mod(LOOP_MERGER_SOUTH_OFFSET - ROUTER_STUB_LEN)
const LOOP_MERGER_NORTH_OFFSET = mod(LOOP_MERGER_SOUTH_OFFSET - ROUTER_STUB_LEN)
const BELT_CIF_TO_LOOP_MERGER = mod(LOOP_MERGER_WEST_OFFSET - BELT_CIF_TO_LOOP_MERGER_LEN)
const BELT_POSTCI_SOUTH_TO_LOOP_MERGER = mod(
  LOOP_MERGER_NORTH_OFFSET - BELT_POSTCI_SOUTH_TO_LOOP_MERGER_LEN,
)

// Ready-for-Review intake chain: intake_splitter.south → rfr_splitter
// → rfr_pole_2 → review-merger.south. Anchor at RM.south wall and
// walk backward through pole_2 (turn arc) and the rfr_splitter's
// stubs+body (instead of the old single turn arc). Chevron jump
// lands at the intake splitter's south face (the only stub UV in
// the chain we don't compute).
const BELT_RFR_POLE2_TO_RM = mod(RM_SOUTH_OFFSET - BELT_RFR_POLE2_TO_RM_LEN)
const RFR_POLE2_OFFSET = mod(BELT_RFR_POLE2_TO_RM - TURN_ARC_LENGTH)
const BELT_RFR_SPLITTER_TO_RFR_POLE2 = mod(RFR_POLE2_OFFSET - BELT_RFR_SPLITTER_TO_RFR_POLE2_LEN)
const RFR_SPLITTER_EAST_OFFSET = mod(BELT_RFR_SPLITTER_TO_RFR_POLE2 - ROUTER_STUB_LEN) // body UV
const RFR_SPLITTER_NORTH_OFFSET = mod(RFR_SPLITTER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV
const RFR_SPLITTER_SOUTH_OFFSET = RFR_SPLITTER_EAST_OFFSET // body-continuous
const BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_SPLITTER = mod(
  RFR_SPLITTER_NORTH_OFFSET - BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_SPLITTER_LEN,
)
const INTAKE_SPLITTER_SOUTH_OFFSET = mod(
  BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_SPLITTER - ROUTER_STUB_LEN,
)

// Bottom chain: rfr_splitter.south → bottom_pole → bottom_merger →
// Closed. Forward-anchor the upstream half from the RFR splitter's
// south wall; backward-anchor the downstream half from Closed.west
// (UV 0) so the visible station face stays clean. Chevron jumps
// land at the merger's west and north wall planes — the port frame
// camouflages the small phase shift.
const BELT_RFR_SPLITTER_SOUTH_TO_BOTTOM_POLE = mod(RFR_SPLITTER_SOUTH_OFFSET + ROUTER_STUB_LEN)
const BOTTOM_POLE_OFFSET = mod(
  BELT_RFR_SPLITTER_SOUTH_TO_BOTTOM_POLE + BELT_RFR_SPLITTER_SOUTH_TO_BOTTOM_POLE_LEN,
)
const BELT_BOTTOM_POLE_TO_BOTTOM_MERGER = mod(BOTTOM_POLE_OFFSET + TURN_ARC_LENGTH)

// Closed-line backward chain. Anchor at Closed.west (UV 0) so the
// terminal station's visible face stays clean; walk back through
// RC_TAIL_MERGER, CR_MERGER, and bottom_merger. Chevron jumps land
// at the merger input walls (small, hidden by the port frames).
const BELT_RC_TAIL_MERGER_TO_CLOSED = mod(-BELT_RC_TAIL_MERGER_TO_CLOSED_LEN)
const RC_TAIL_MERGER_EAST_OFFSET = mod(BELT_RC_TAIL_MERGER_TO_CLOSED - ROUTER_STUB_LEN) // body UV
const RC_TAIL_MERGER_WEST_OFFSET = mod(RC_TAIL_MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV (input)
const RC_TAIL_MERGER_NORTH_OFFSET = RC_TAIL_MERGER_WEST_OFFSET // body-continuous (input)
const BELT_CR_MERGER_TO_RC_TAIL_MERGER = mod(
  RC_TAIL_MERGER_WEST_OFFSET - BELT_CR_MERGER_TO_RC_TAIL_MERGER_LEN,
)
const CR_MERGER_EAST_OFFSET = mod(BELT_CR_MERGER_TO_RC_TAIL_MERGER - ROUTER_STUB_LEN) // body UV
const CR_MERGER_WEST_OFFSET = mod(CR_MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV (input)
const CR_MERGER_NORTH_OFFSET = CR_MERGER_WEST_OFFSET // body-continuous (input)
const BELT_MC_FOOT_MERGER_TO_CR_MERGER = mod(
  CR_MERGER_WEST_OFFSET - BELT_MC_FOOT_MERGER_TO_CR_MERGER_LEN,
)
const MC_FOOT_MERGER_EAST_OFFSET = mod(BELT_MC_FOOT_MERGER_TO_CR_MERGER - ROUTER_STUB_LEN) // body UV
const MC_FOOT_MERGER_WEST_OFFSET = mod(MC_FOOT_MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV (input)
const MC_FOOT_MERGER_NORTH_OFFSET = MC_FOOT_MERGER_WEST_OFFSET // body-continuous (input)
const BELT_BOTTOM_MERGER_TO_MC_FOOT_MERGER = mod(
  MC_FOOT_MERGER_WEST_OFFSET - BELT_BOTTOM_MERGER_TO_MC_FOOT_MERGER_LEN,
)
const BOTTOM_MERGER_EAST_OFFSET = mod(BELT_BOTTOM_MERGER_TO_MC_FOOT_MERGER - ROUTER_STUB_LEN) // body UV
const BOTTOM_MERGER_WEST_OFFSET = mod(BOTTOM_MERGER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV (input)
const BOTTOM_MERGER_NORTH_OFFSET = BOTTOM_MERGER_WEST_OFFSET // body-continuous (input)

// CR splitter forward-chain: CR.east (UV 4) → 1-cell belt →
// CR_SPLITTER body. South stub continues into the vertical drop
// to CR_MERGER; north stub climbs to RC_MERGER's south input;
// chevron jumps land at the merger walls.
const BELT_CR_TO_CR_SPLITTER = STATION_EAST_UV_AT_WALL
const CR_SPLITTER_WEST_OFFSET = mod(BELT_CR_TO_CR_SPLITTER + BELT_CR_TO_CR_SPLITTER_LEN)
const CR_SPLITTER_EAST_OFFSET = mod(CR_SPLITTER_WEST_OFFSET + ROUTER_STUB_LEN) // body UV
const CR_SPLITTER_SOUTH_OFFSET = CR_SPLITTER_EAST_OFFSET // body-continuous
const CR_SPLITTER_NORTH_OFFSET = CR_SPLITTER_EAST_OFFSET // body-continuous
const BELT_CR_SPLITTER_SOUTH_TO_CR_MERGER = mod(CR_SPLITTER_SOUTH_OFFSET + ROUTER_STUB_LEN)
const BELT_CR_SPLITTER_NORTH_TO_RC_MERGER = mod(CR_SPLITTER_NORTH_OFFSET + ROUTER_STUB_LEN)

// RC chain: forward-anchor from RC.east (UV 4). RC_MERGER body is
// shared by west input + south input + east output. Items then
// reach RC_SPLITTER (west input + north output + south output).
// North output goes through 2 corner poles (under-bridge route),
// south output drops to RC_TAIL_MERGER on the Closed line.
const BELT_RC_TO_RC_MERGER = STATION_EAST_UV_AT_WALL
const RC_MERGER_WEST_OFFSET = mod(BELT_RC_TO_RC_MERGER + BELT_RC_TO_RC_MERGER_LEN)
const RC_MERGER_EAST_OFFSET = mod(RC_MERGER_WEST_OFFSET + ROUTER_STUB_LEN) // body UV
const RC_MERGER_SOUTH_OFFSET = RC_MERGER_WEST_OFFSET // body-continuous (input)
const BELT_RC_MERGER_TO_RC_SPLITTER = mod(RC_MERGER_EAST_OFFSET + ROUTER_STUB_LEN)
const RC_SPLITTER_WEST_OFFSET = mod(
  BELT_RC_MERGER_TO_RC_SPLITTER + BELT_RC_MERGER_TO_RC_SPLITTER_LEN,
)
const RC_SPLITTER_EAST_OFFSET = mod(RC_SPLITTER_WEST_OFFSET + ROUTER_STUB_LEN) // body UV
const RC_SPLITTER_NORTH_OFFSET = RC_SPLITTER_EAST_OFFSET // body-continuous
const RC_SPLITTER_SOUTH_OFFSET = RC_SPLITTER_EAST_OFFSET // body-continuous

// Under-bridge route: north stub of RC_SPLITTER → 1-cell vertical
// → BRIDGE_UNDER_POLE_1 (turn west) → 1-cell horizontal beneath
// the bridge → BRIDGE_UNDER_POLE_2 (turn north) → 4-cell vertical
// climb (passing through the bridge's column at floor-z) →
// OVER_BRIDGE_MERGER.south input.
const BELT_RC_SPLITTER_NORTH_TO_UNDER_POLE_1 = mod(RC_SPLITTER_NORTH_OFFSET + ROUTER_STUB_LEN)
const BRIDGE_UNDER_POLE_1_OFFSET = mod(
  BELT_RC_SPLITTER_NORTH_TO_UNDER_POLE_1 + BELT_RC_SPLITTER_NORTH_TO_UNDER_POLE_1_LEN,
)
const BELT_UNDER_POLE_1_TO_UNDER_POLE_2 = mod(BRIDGE_UNDER_POLE_1_OFFSET + TURN_ARC_LENGTH)
const BRIDGE_UNDER_POLE_2_OFFSET = mod(
  BELT_UNDER_POLE_1_TO_UNDER_POLE_2 + BELT_UNDER_POLE_1_TO_UNDER_POLE_2_LEN,
)
const BELT_UNDER_POLE_2_TO_OVER_BRIDGE_MERGER = mod(BRIDGE_UNDER_POLE_2_OFFSET + TURN_ARC_LENGTH)
const BELT_UNDER_POLE_2_TO_OVER_BRIDGE_MERGER_LEN =
  (OVER_BRIDGE_MERGER_ROW - (BRIDGE_UNDER_POLE_2_ROW + 1)) * FLOOR_CELL // 320
const OVER_BRIDGE_MERGER_SOUTH_OFFSET = mod(
  BELT_UNDER_POLE_2_TO_OVER_BRIDGE_MERGER + BELT_UNDER_POLE_2_TO_OVER_BRIDGE_MERGER_LEN,
) // wall UV (input)
const OVER_BRIDGE_MERGER_WEST_OFFSET = mod(OVER_BRIDGE_MERGER_SOUTH_OFFSET + ROUTER_STUB_LEN) // body UV (output)
const OVER_BRIDGE_MERGER_EAST_OFFSET = OVER_BRIDGE_MERGER_SOUTH_OFFSET // body-continuous (input)

// South branch of RC_SPLITTER feeds RC_TAIL_MERGER's north input.
// Forward-anchored off the splitter's south wall; chevron jump
// lands at the merger's north port frame.
const BELT_RC_SPLITTER_SOUTH_TO_RC_TAIL_MERGER = mod(RC_SPLITTER_SOUTH_OFFSET + ROUTER_STUB_LEN)

// Review Approved → Merged bridge. Forward-anchor from RA's east
// stub (UV 4). The bridge butts directly against RA_SPLITTER's
// west wall — chevron jump lands at the bridge↔splitter junction
// (port frame hides it). Splitter and downstream chain are
// backward-anchored from Merged.west (UV 0) so the visible
// terminal station face stays clean.
const BELT_RA_BRIDGE_PATH_OFFSET = STATION_EAST_UV_AT_WALL
const BELT_RA_SPLITTER_TO_MERGED_LEN = (MERGED_COL - (RA_SPLITTER_COL + 1)) * FLOOR_CELL // 80
const BELT_RA_SPLITTER_TO_MERGED = mod(-BELT_RA_SPLITTER_TO_MERGED_LEN)
const RA_SPLITTER_EAST_OFFSET = mod(BELT_RA_SPLITTER_TO_MERGED - ROUTER_STUB_LEN) // body UV
const RA_SPLITTER_WEST_OFFSET = mod(RA_SPLITTER_EAST_OFFSET - ROUTER_STUB_LEN) // wall UV (input)
const RA_SPLITTER_NORTH_OFFSET = RA_SPLITTER_EAST_OFFSET // body-continuous (output)

// Splitter.north → 2-cell vertical → OVER_BRIDGE_POLE → 1-cell
// horizontal → OVER_BRIDGE_MERGER.east. Forward-anchor from the
// splitter; chevron jump (if any) lands at the merger's east port.
// The horizontal pole→merger belt's length is implicit in the port
// positions and doesn't enter the chevron math.
const BELT_RA_SPLITTER_NORTH_TO_OVER_BRIDGE_POLE_LEN =
  (OVER_BRIDGE_POLE_ROW - (RA_SPLITTER_ROW + 1)) * FLOOR_CELL // 160
const BELT_RA_SPLITTER_NORTH_TO_OVER_BRIDGE_POLE = mod(RA_SPLITTER_NORTH_OFFSET + ROUTER_STUB_LEN)
const OVER_BRIDGE_POLE_OFFSET = mod(
  BELT_RA_SPLITTER_NORTH_TO_OVER_BRIDGE_POLE + BELT_RA_SPLITTER_NORTH_TO_OVER_BRIDGE_POLE_LEN,
)
const BELT_OVER_BRIDGE_POLE_TO_OVER_BRIDGE_MERGER = mod(OVER_BRIDGE_POLE_OFFSET + TURN_ARC_LENGTH)

// Merge Conflicts chain. Forward-anchor from MC.east (UV 4) through
// MC_SPLITTER → 2-cell vertical → MC_TAIL_MERGER's south input.
// East+south outputs of the splitter are dead-end stubs reserved
// for the future MC → Closed connection. OVER_BRIDGE_MERGER's west
// output also feeds the merger via a long row-26 belt — anchored
// forward from OVER_BRIDGE_MERGER.west's wall UV; chevron jump
// (if any) lands at MC_TAIL_MERGER's east wall.
const BELT_MC_TO_MC_SPLITTER_LEN = (MC_SPLITTER_COL - (DEST_COL + STATION_W)) * FLOOR_CELL // 80
const BELT_MC_SPLITTER_NORTH_TO_MC_TAIL_MERGER_LEN =
  (MC_TAIL_MERGER_ROW - (MC_SPLITTER_ROW + 1)) * FLOOR_CELL // 160

const BELT_MC_TO_MC_SPLITTER = STATION_EAST_UV_AT_WALL
const MC_SPLITTER_WEST_OFFSET = mod(BELT_MC_TO_MC_SPLITTER + BELT_MC_TO_MC_SPLITTER_LEN) // wall UV
const MC_SPLITTER_EAST_OFFSET = mod(MC_SPLITTER_WEST_OFFSET + ROUTER_STUB_LEN) // body UV
const MC_SPLITTER_NORTH_OFFSET = MC_SPLITTER_EAST_OFFSET // body-continuous
const MC_SPLITTER_SOUTH_OFFSET = MC_SPLITTER_EAST_OFFSET // body-continuous

const BELT_MC_SPLITTER_NORTH_TO_MC_TAIL_MERGER = mod(MC_SPLITTER_NORTH_OFFSET + ROUTER_STUB_LEN)
const MC_TAIL_MERGER_SOUTH_OFFSET = mod(
  BELT_MC_SPLITTER_NORTH_TO_MC_TAIL_MERGER + BELT_MC_SPLITTER_NORTH_TO_MC_TAIL_MERGER_LEN,
) // wall UV (input)
const MC_TAIL_MERGER_WEST_OFFSET = mod(MC_TAIL_MERGER_SOUTH_OFFSET + ROUTER_STUB_LEN) // body UV (output)
const MC_TAIL_MERGER_EAST_OFFSET = MC_TAIL_MERGER_SOUTH_OFFSET // body-continuous (input)

// OVER_BRIDGE_MERGER.west wall UV → 18-cell belt → MC_TAIL_MERGER.east.
const BELT_OVER_BRIDGE_MERGER_TO_MC_TAIL_MERGER = mod(
  OVER_BRIDGE_MERGER_WEST_OFFSET + ROUTER_STUB_LEN,
)

// Row-26 back-to-NC chain. MC_TAIL_MERGER.west → 11-cell belt →
// NC_LOOP_MERGER.east → body → NC_LOOP_MERGER.west → 7-cell belt →
// BACK_TO_NC_POLE.east → turn arc → 6-cell vertical drop → NC
// merger.north. The NC loop merger also catches S1's new north
// output via a 6-cell vertical climb from row 19 up to row 26.
// Forward-anchor from MC_TAIL_MERGER's body UV; chevron jumps land
// at NC_LOOP_MERGER.south wall (where S1's chain meets it) and at
// the NC merger's north port frame.
const BELT_MC_TAIL_MERGER_TO_NC_LOOP_MERGER_LEN =
  (MC_TAIL_MERGER_COL - (NC_LOOP_MERGER_COL + 1)) * FLOOR_CELL // 880
const BELT_MC_TAIL_MERGER_TO_NC_LOOP_MERGER = mod(MC_TAIL_MERGER_WEST_OFFSET + ROUTER_STUB_LEN)
const NC_LOOP_MERGER_EAST_OFFSET = mod(
  BELT_MC_TAIL_MERGER_TO_NC_LOOP_MERGER + BELT_MC_TAIL_MERGER_TO_NC_LOOP_MERGER_LEN,
) // wall UV (input)
const NC_LOOP_MERGER_WEST_OFFSET = mod(NC_LOOP_MERGER_EAST_OFFSET + ROUTER_STUB_LEN) // body UV (output)
const NC_LOOP_MERGER_SOUTH_OFFSET = NC_LOOP_MERGER_EAST_OFFSET // body-continuous (input)

const BELT_NC_LOOP_MERGER_TO_BACK_TO_NC_POLE_LEN =
  (NC_LOOP_MERGER_COL - (BACK_TO_NC_POLE_COL + 1)) * FLOOR_CELL // 560
const BELT_NC_LOOP_MERGER_TO_BACK_TO_NC_POLE = mod(NC_LOOP_MERGER_WEST_OFFSET + ROUTER_STUB_LEN)
const BACK_TO_NC_POLE_OFFSET = mod(
  BELT_NC_LOOP_MERGER_TO_BACK_TO_NC_POLE + BELT_NC_LOOP_MERGER_TO_BACK_TO_NC_POLE_LEN,
)
const BELT_BACK_TO_NC_POLE_TO_MERGER = mod(BACK_TO_NC_POLE_OFFSET + TURN_ARC_LENGTH)

// S1.north → 6-cell vertical climb → NC_LOOP_MERGER.south. Forward-
// anchored off S1's north wall; chevron jump lands at the merger's
// south port frame.
const BELT_S1_NORTH_TO_NC_LOOP_MERGER = mod(S1_NORTH_OFFSET + ROUTER_STUB_LEN)

// ─── Station specs ────────────────────────────────────────────────

const stationSpec = (
  col: number,
  row: number,
  queued: number,
  runs: number,
  label: string,
  eventType: string,
): Station => ({
  x: col * FLOOR_CELL,
  y: row * FLOOR_CELL,
  z: 0,
  w: STATION_W * FLOOR_CELL,
  d: STATION_D * FLOOR_CELL,
  h: STATION_H,
  // The station's stable id IS its event_type. Click hit-testing,
  // the snapshot reconciler, and the routing table all key off this
  // value, so /api/factory/snapshot's `stations[event_type]` map
  // lines up 1:1 with our 12 stations without an intermediate slug
  // layer.
  id: eventType,
  queuedCount: queued,
  runCount: runs,
  label,
  ports: [
    { kind: 'input', direction: 'west', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
    { kind: 'output', direction: 'east', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
  ],
})

const PR_OPENED = stationSpec(PR_OPENED_COL, PR_OPENED_ROW, 1, 0, 'PR Opened', 'github:pr:opened')
const READY_FOR_REVIEW = stationSpec(
  READY_FOR_REVIEW_COL,
  READY_FOR_REVIEW_ROW,
  2,
  1,
  'Ready for Review',
  'github:pr:ready_for_review',
)
const NEW_COMMITS = stationSpec(NC_COL, NC_ROW, 4, 2, 'New Commits', 'github:pr:new_commits')
const MERGE_CONFLICTS = stationSpec(
  DEST_COL,
  MC_ROW,
  1,
  0,
  'Merge Conflicts',
  'github:pr:conflicts',
)
const CI_PASSED = stationSpec(
  DEST_COL,
  CI_PASSED_ROW,
  0,
  0,
  'CI Passed',
  'github:pr:ci_check_passed',
)
const CI_FAILED = stationSpec(
  DEST_COL,
  CI_FAILED_ROW,
  2,
  1,
  'CI Failed',
  'github:pr:ci_check_failed',
)
const REVIEW_REQUESTED = stationSpec(
  RR_COL,
  RR_ROW,
  3,
  1,
  'Review Requested',
  'github:pr:review_requested',
)
const REVIEW_APPROVED = stationSpec(
  REVIEW_DEST_COL,
  REVIEW_APPROVED_ROW,
  0,
  0,
  'Review Approved',
  'github:pr:review_approved',
)
const REVIEW_COMMENTED = stationSpec(
  REVIEW_DEST_COL,
  REVIEW_COMMENTED_ROW,
  1,
  0,
  'Review Commented',
  'github:pr:review_commented',
)
const CHANGES_REQUESTED = stationSpec(
  REVIEW_DEST_COL,
  CHANGES_REQUESTED_ROW,
  2,
  0,
  'Changes Requested',
  'github:pr:review_changes_requested',
)
const CLOSED = stationSpec(CLOSED_COL, CLOSED_ROW, 2, 0, 'Closed', 'github:pr:closed')
const MERGED = stationSpec(MERGED_COL, MERGED_ROW, 1, 0, 'Merged', 'github:pr:merged')

// ─── Mergers / Splitters ──────────────────────────────────────────

// Merger: 2 inputs (west = spawner intake, south = CI Failed
// loopback), 1 output (east → NC). Same primitive as Router.
// Intake splitter: 1 input west (spawner appears here), 2 outputs
// — east continues to the NC merger; south drops into the
// ready-for-review intake chain.
const INTAKE_SPLITTER: Router = {
  col: INTAKE_SPLITTER_COL,
  row: INTAKE_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

const MERGER: Router = {
  col: MERGER_COL,
  row: MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Back-to-NC pole: turn pole at (col 13, row 26). Catches the
// row-26 west-flowing return chain via its east input and
// drops it south into the NC merger's new north input.
const BACK_TO_NC_POLE: Pole = {
  col: BACK_TO_NC_POLE_COL,
  row: BACK_TO_NC_POLE_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

// nc_loop_merger: sits on the row-26 back-to-NC belt directly
// above S1. East in (from MC_TAIL_MERGER chain), south in (from
// S1's new north output), west out (continues to BACK_TO_NC_POLE).
const NC_LOOP_MERGER: Router = {
  col: NC_LOOP_MERGER_COL,
  row: NC_LOOP_MERGER_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

const SPLITTER_1: Router = {
  col: S1_COL,
  row: SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    // South branch feeds the bridge → bottom_merger → Closed.
    { direction: 'south', kind: 'output' },
    // North branch climbs to NC_LOOP_MERGER for the NC→NC short
    // loop ("more commits arrive while NC is still digesting").
    { direction: 'north', kind: 'output' },
  ],
}

const SPLITTER_2: Router = {
  col: S2_COL,
  row: SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// Post-CI: 1 input west (from CI Passed), 3 outputs N/E/S — all
// unwired today. Three outputs leave room for the eventual
// "review continues / loop back / fast-merge" decision.
const POST_CI_SPLITTER: Router = {
  col: POST_CI_COL,
  row: POST_CI_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// Review-intake merger: 2 inputs (west = post-CI east, south =
// changes_requested loopback target — unwired today), 1 output
// (east → Review Requested).
const REVIEW_MERGER: Router = {
  col: RM_COL,
  row: RM_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Review-outcomes splitter: 1 input (west, from RR), 3 outputs
// fanning to the three review states.
const REVIEW_SPLITTER: Router = {
  col: REVIEW_COL,
  row: REVIEW_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// ─── Poles (90° turns) ────────────────────────────────────────────

// "Above" sits at high-row (row 19) — visually above S2. S2 is at
// lower row, so the belt arrives on the pole's SOUTH face and
// arcs east to MC.
const POLE_ABOVE: Pole = {
  col: S2_COL,
  row: POLE_ABOVE_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// "Below" sits at low-row (row 11). S2 is at higher row, so the
// belt arrives on the pole's NORTH face and arcs east to CI
// Failed.
const POLE_BELOW: Pole = {
  col: S2_COL,
  row: POLE_BELOW_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Review fan poles — mirror of CI fan poles but at REVIEW_COL.
const POLE_REVIEW_ABOVE: Pole = {
  col: REVIEW_COL,
  row: POLE_REVIEW_ABOVE_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

const POLE_REVIEW_BELOW: Pole = {
  col: REVIEW_COL,
  row: POLE_REVIEW_BELOW_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// ─── Loopback elements ───────────────────────────────────────────
//
// A merger and two corner poles wrap CI Failed.east (and the
// post-CI splitter's south output) around the bottom of the
// floor and back into the NC merger's south input.
//
// loop_merger: just east of CI Failed. West in (from CI Failed.east),
//   north in (from post-CI splitter.south), south out (toward the
//   bottom-east corner pole).
const LOOP_MERGER: Router = {
  col: LOOP_MERGER_COL,
  row: LOOP_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

// pole_2: bottom-east corner. North in (from loop_merger), west out.
const LOOP_POLE_2: Pole = {
  col: LOOP_POLE2_COL,
  row: LOOP_POLE2_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

// pole_3: bottom-west corner. East in (from pole_2), north out
// (toward merger.south).
const LOOP_POLE_3: Pole = {
  col: LOOP_POLE3_COL,
  row: LOOP_POLE3_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'north', kind: 'output' },
  ],
}

// ─── Ready-for-Review intake corners ──────────────────────────────
//
// rfr_splitter: directly below the intake splitter. North in (from
//   intake_splitter.south), east out (toward rfr_pole_2 across row
//   11), south out (drops to the bottom_pole on the new row-8 chain
//   that eventually leads to Closed).
const RFR_SPLITTER: Router = {
  col: RFR_SPLITTER_COL,
  row: RFR_SPLITTER_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// rfr_pole_2: directly below RM. West in (from rfr_splitter.east),
//   north out (climbing into RM.south).
const RFR_POLE_2: Pole = {
  col: RFR_POLE2_COL,
  row: RFR_POLE2_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
  ],
}

// ─── Bottom chain ─────────────────────────────────────────────────
//
// bottom_pole: directly below rfr_splitter at col 11, row 8. North
//   in (from rfr_splitter.south), east out (running along row 8 to
//   the bottom merger). Avoids the row-11 RFR-intake belt by
//   sitting south of every existing horizontal stretch.
const BOTTOM_POLE: Pole = {
  col: BOTTOM_POLE_COL,
  row: BOTTOM_POLE_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// bottom_merger: directly below the bridge's south end at col 21,
// row 8. North in (from bridge.south, zero-length connector),
// west in (from bottom_pole.east), east out (toward CR_MERGER).
const BOTTOM_MERGER: Router = {
  col: BOTTOM_MERGER_COL,
  row: BOTTOM_MERGER_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// cr_splitter: 1 cell east of Changes Requested. Catches CR.east,
// fans east (reserved for future destination), south (drops to
// CR_MERGER on the row-8 belt), and north (climbs into the new
// RC_MERGER on the row-19 line).
const CR_SPLITTER: Router = {
  col: CR_SPLITTER_COL,
  row: CR_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
    { direction: 'north', kind: 'output' },
  ],
}

// rc_merger: 1 cell east of Review Commented. Combines RC.east
// (west input) with the CR_SPLITTER north output (south input)
// and pushes both east into the trailing pole.
const RC_MERGER: Router = {
  col: RC_MERGER_COL,
  row: RC_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// rc_splitter: replaces the old rc_pole at (col 53, row 19). Catches
// RC_MERGER.east via the west input; fans north (under-bridge route)
// and south (drops to RC_TAIL_MERGER on the Closed line).
const RC_SPLITTER: Router = {
  col: RC_SPLITTER_COL,
  row: RC_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// Under-bridge corner poles. POLE_1 takes the splitter's vertical
// north output and turns it west. POLE_2 catches that westbound
// flow and turns it UP — its north output is a dead-end stub for
// a future destination on the far side of the bridge.
const BRIDGE_UNDER_POLE_1: Pole = {
  col: BRIDGE_UNDER_POLE_1_COL,
  row: BRIDGE_UNDER_POLE_1_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

const BRIDGE_UNDER_POLE_2: Pole = {
  col: BRIDGE_UNDER_POLE_2_COL,
  row: BRIDGE_UNDER_POLE_2_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'north', kind: 'output' },
  ],
}

// rc_tail_merger: inserted on the bottom_merger → Closed belt at
// col 53. West in (from CR_MERGER), north in (from RC_SPLITTER's
// south drop), east out (final hop to Closed).
const RC_TAIL_MERGER: Router = {
  col: RC_TAIL_MERGER_COL,
  row: RC_TAIL_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// over_bridge_merger: 5 cells north of BRIDGE_UNDER_POLE_2 at
// col 51. South in (from the under-bridge climb), east in
// (placeholder for a future feed), west out (faces left, exits
// toward the rest of the layout).
const OVER_BRIDGE_MERGER: Router = {
  col: OVER_BRIDGE_MERGER_COL,
  row: OVER_BRIDGE_MERGER_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

// ra_splitter: sits on the flat connector right after the RA →
// Merged bridge (col 53, row 23). Even an approved review sometimes
// lands more commits before merge — this splitter forks that case
// off. East output continues to Merged; north output drops into
// the over-bridge pole and feeds the over-bridge merger.
const RA_SPLITTER: Router = {
  col: RA_SPLITTER_COL,
  row: RA_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'east', kind: 'output' },
    { direction: 'north', kind: 'output' },
  ],
}

// over_bridge_pole: 1 cell east of OVER_BRIDGE_MERGER at (col 53,
// row 26). Catches the splitter's south-flowing branch and turns
// it west into the merger.
const OVER_BRIDGE_POLE: Pole = {
  col: OVER_BRIDGE_POLE_COL,
  row: OVER_BRIDGE_POLE_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

// mc_splitter: 1 cell east of Merge Conflicts. West input catches
// MC.east. North output drops into mc_tail_merger (active branch
// toward the back-to-NC path). East and south outputs stay
// dead-end stubs — reserved for the future MC → Closed connection.
const MC_SPLITTER: Router = {
  col: MC_SPLITTER_COL,
  row: MC_SPLITTER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'east', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

// mc_tail_merger: same row as OVER_BRIDGE_MERGER, sitting at
// col 31 above MC_SPLITTER. South input catches the MC chain;
// east input is a placeholder for the eventual incoming feed
// from OVER_BRIDGE_MERGER's west output; west output is dormant
// — it will continue west on the path back to New Commits.
const MC_TAIL_MERGER: Router = {
  col: MC_TAIL_MERGER_COL,
  row: MC_TAIL_MERGER_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

// mc_foot_merger: lands the col-33 MC south branch on the row-8
// Closed line. West in (from bottom_merger), north in (from the
// 2-bridge descent), east out (continues toward CR_MERGER and
// eventually Closed).
const MC_FOOT_MERGER: Router = {
  col: MC_FOOT_MERGER_COL,
  row: MC_FOOT_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// cr_merger: sits on the bottom_merger → Closed belt at col 51.
// West in (from bottom_merger), north in (from CR splitter chain),
// east out (continuing toward Closed).
const CR_MERGER: Router = {
  col: CR_MERGER_COL,
  row: CR_MERGER_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// ─── PR Opened intake poles ───────────────────────────────────────
//
// Z-shape wrapping under PR Opened. Conveyors leave PR Opened
// east, drop to row 23 below the station, run west under it, drop
// further to row 19, then run east into RFR.west.
const PRO_POLE_1: Pole = {
  col: PRO_POLE1_COL,
  row: PRO_POLE1_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

const PRO_POLE_2: Pole = {
  col: PRO_POLE2_COL,
  row: PRO_POLE2_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'west', kind: 'output' },
  ],
}

const PRO_POLE_3: Pole = {
  col: PRO_POLE3_COL,
  row: PRO_POLE3_ROW,
  ports: [
    { direction: 'east', kind: 'input' },
    { direction: 'south', kind: 'output' },
  ],
}

const PRO_POLE_4: Pole = {
  col: PRO_POLE4_COL,
  row: PRO_POLE4_ROW,
  ports: [
    { direction: 'north', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

export interface CameraStateForHUD {
  /** Polar angle from +z axis, in radians. 0 = top-down. */
  pitch: number
  /** Azimuth around +z axis, in radians. */
  yaw: number
  /** Zoom factor relative to the initial view. >1 = zoomed in. */
  zoom: number
}

export interface ClickedStationInfo {
  /** The station's event_type (its stable id). */
  id: string
  label: string
  queuedCount: number
  runCount: number
  /** Entities parked at this station with no active run. Drives
   *  the drawer's intake-tray chip list. */
  queued: FactoryEntity[]
  /** Active runs at this station — same shape as the snapshot's
   *  `stations[event_type].runs` array. Drives the drawer's main-
   *  tray chip list. */
  runs: FactoryStation['runs']
}

export interface IsoSceneHandle {
  destroy: () => void
  resetView: () => void
  /** Subscribe to camera state changes. Returns an unsubscribe
   *  function. Used by debug HUDs (none currently active in
   *  production, but the hook stays for future overlay work). */
  onCameraChange: (cb: (s: CameraStateForHUD) => void) => () => void
  /** Subscribe to station picks. Fires with the clicked station's
   *  full data (counts + queued + runs), or `null` when the click
   *  landed off-station. Drawers use the null signal to close.
   *  Returns an unsubscribe function. */
  onStationClick: (cb: (info: ClickedStationInfo | null) => void) => () => void
  /** Subscribe to live data changes for a single station — the
   *  drawer's content stream while it's open. The reconciler fires
   *  the callback whenever queued entities or runs at the station
   *  change (count or content); idle frames are deduplicated by a
   *  per-station hash so consumers don't re-render on no-ops.
   *  Returns an unsubscribe function. */
  onStationDataChange: (stationId: string, cb: (info: ClickedStationInfo) => void) => () => void
  /** Hand the scene the latest /api/factory/snapshot payload. The
   *  scene's per-frame reconciler then projects every entity to
   *  either a station (driving tray counts) or a transit (driving
   *  chip meshes). Backend is the source of truth for both. */
  applySnapshot: (snapshot: FactorySnapshot) => void
  /** Enter ambient cinematic mode — the camera detaches from user
   *  input and begins an eased, auto-advancing scenic tour. `reduced`
   *  (from prefers-reduced-motion) collapses it to a single slow drift. */
  enterCinematic: (reduced?: boolean) => void
  /** Leave cinematic mode — the camera eases back to the pose held
   *  before entering, then user controls re-attach. No-op when not
   *  active. */
  exitCinematic: () => void
  /** Fires once the camera has fully eased back after an exit, so the
   *  HUD doesn't return mid-move. (The V1 director never self-exits.)
   *  Returns an unsubscribe. */
  onCinematicEnd: (cb: () => void) => () => void
}

export async function createIsoScene(container: HTMLDivElement): Promise<IsoSceneHandle> {
  const canvas = document.createElement('canvas')
  canvas.style.width = '100%'
  canvas.style.height = '100%'
  canvas.style.display = 'block'
  canvas.style.touchAction = 'none'
  container.appendChild(canvas)

  // Black overlay the cinematic director fades through for its
  // Forza-style cut transitions. Above the canvas, non-interactive; the
  // director owns its opacity (0 clear … 1 black). Inline-styled to match
  // the canvas — this layer is scene-managed, not React-managed.
  if (getComputedStyle(container).position === 'static') container.style.position = 'relative'
  const fadeOverlay = document.createElement('div')
  fadeOverlay.setAttribute('aria-hidden', 'true')
  fadeOverlay.style.position = 'absolute'
  fadeOverlay.style.inset = '0'
  fadeOverlay.style.background = '#000'
  fadeOverlay.style.opacity = '0'
  fadeOverlay.style.pointerEvents = 'none'
  fadeOverlay.style.zIndex = '1'
  container.appendChild(fadeOverlay)

  const initialRect = container.getBoundingClientRect()
  const dpr = window.devicePixelRatio || 1
  canvas.width = initialRect.width * dpr
  canvas.height = initialRect.height * dpr

  const renderer = new IsoScene(canvas)
  renderer.buildFloor(FLOOR_SIZE, FLOOR_CELL, false)

  // ─── Build geometry ────────────────────────────────────────────
  const prOpened = renderer.addStation(PR_OPENED)
  const readyForReview = renderer.addStation(READY_FOR_REVIEW)
  const newCommits = renderer.addStation(NEW_COMMITS)
  const mergeConflicts = renderer.addStation(MERGE_CONFLICTS)
  const ciPassed = renderer.addStation(CI_PASSED)
  const ciFailed = renderer.addStation(CI_FAILED)
  const reviewRequested = renderer.addStation(REVIEW_REQUESTED)
  const reviewApproved = renderer.addStation(REVIEW_APPROVED)
  const reviewCommented = renderer.addStation(REVIEW_COMMENTED)
  const changesRequested = renderer.addStation(CHANGES_REQUESTED)
  const closed = renderer.addStation(CLOSED)
  const merged = renderer.addStation(MERGED)

  const intakeSplitter = renderer.addRouter(INTAKE_SPLITTER, FLOOR_CELL, {
    west: INTAKE_SPLITTER_WEST_OFFSET,
    east: INTAKE_SPLITTER_EAST_OFFSET,
    south: INTAKE_SPLITTER_SOUTH_OFFSET,
  })
  const merger = renderer.addRouter(MERGER, FLOOR_CELL, {
    west: MERGER_WEST_OFFSET,
    south: MERGER_SOUTH_OFFSET,
    north: MERGER_NORTH_OFFSET,
    east: MERGER_EAST_OFFSET,
  })
  const splitter1 = renderer.addRouter(SPLITTER_1, FLOOR_CELL, {
    west: S1_WEST_OFFSET,
    east: S1_EAST_OFFSET,
    south: S1_SOUTH_OFFSET,
    north: S1_NORTH_OFFSET,
  })
  const splitter2 = renderer.addRouter(SPLITTER_2, FLOOR_CELL, {
    west: S2_WEST_OFFSET,
    north: S2_NORTH_OFFSET,
    east: S2_EAST_OFFSET,
    south: S2_SOUTH_OFFSET,
  })
  const postCi = renderer.addRouter(POST_CI_SPLITTER, FLOOR_CELL, {
    west: POST_CI_WEST_OFFSET,
    north: POST_CI_OUTPUT_OFFSET,
    east: POST_CI_OUTPUT_OFFSET,
    south: POST_CI_OUTPUT_OFFSET,
  })
  const reviewMerger = renderer.addRouter(REVIEW_MERGER, FLOOR_CELL, {
    west: RM_WEST_OFFSET,
    south: RM_SOUTH_OFFSET,
    east: RM_EAST_OFFSET,
  })
  const reviewSplitter = renderer.addRouter(REVIEW_SPLITTER, FLOOR_CELL, {
    west: REVIEW_WEST_OFFSET,
    north: REVIEW_NORTH_OFFSET,
    east: REVIEW_EAST_OFFSET,
    south: REVIEW_SOUTH_OFFSET,
  })

  const poleAbove = renderer.addPole(POLE_ABOVE, FLOOR_CELL, POLE_ABOVE_OFFSET)
  const poleBelow = renderer.addPole(POLE_BELOW, FLOOR_CELL, POLE_BELOW_OFFSET)
  const poleReviewAbove = renderer.addPole(POLE_REVIEW_ABOVE, FLOOR_CELL, POLE_REVIEW_ABOVE_OFFSET)
  const poleReviewBelow = renderer.addPole(POLE_REVIEW_BELOW, FLOOR_CELL, POLE_REVIEW_BELOW_OFFSET)
  const loopMerger = renderer.addRouter(LOOP_MERGER, FLOOR_CELL, {
    west: LOOP_MERGER_WEST_OFFSET,
    north: LOOP_MERGER_NORTH_OFFSET,
    south: LOOP_MERGER_SOUTH_OFFSET,
  })
  const loopPole2 = renderer.addPole(LOOP_POLE_2, FLOOR_CELL, LOOP_POLE2_OFFSET)
  const loopPole3 = renderer.addPole(LOOP_POLE_3, FLOOR_CELL, LOOP_POLE3_OFFSET)
  const rfrSplitter = renderer.addRouter(RFR_SPLITTER, FLOOR_CELL, {
    north: RFR_SPLITTER_NORTH_OFFSET,
    east: RFR_SPLITTER_EAST_OFFSET,
    south: RFR_SPLITTER_SOUTH_OFFSET,
  })
  const rfrPole2 = renderer.addPole(RFR_POLE_2, FLOOR_CELL, RFR_POLE2_OFFSET)
  const bottomPole = renderer.addPole(BOTTOM_POLE, FLOOR_CELL, BOTTOM_POLE_OFFSET)
  const bottomMerger = renderer.addRouter(BOTTOM_MERGER, FLOOR_CELL, {
    west: BOTTOM_MERGER_WEST_OFFSET,
    north: BOTTOM_MERGER_NORTH_OFFSET,
    east: BOTTOM_MERGER_EAST_OFFSET,
  })
  const crSplitter = renderer.addRouter(CR_SPLITTER, FLOOR_CELL, {
    west: CR_SPLITTER_WEST_OFFSET,
    east: CR_SPLITTER_EAST_OFFSET,
    south: CR_SPLITTER_SOUTH_OFFSET,
    north: CR_SPLITTER_NORTH_OFFSET,
  })
  const crMerger = renderer.addRouter(CR_MERGER, FLOOR_CELL, {
    west: CR_MERGER_WEST_OFFSET,
    north: CR_MERGER_NORTH_OFFSET,
    east: CR_MERGER_EAST_OFFSET,
  })
  const rcMerger = renderer.addRouter(RC_MERGER, FLOOR_CELL, {
    west: RC_MERGER_WEST_OFFSET,
    south: RC_MERGER_SOUTH_OFFSET,
    east: RC_MERGER_EAST_OFFSET,
  })
  const rcSplitter = renderer.addRouter(RC_SPLITTER, FLOOR_CELL, {
    west: RC_SPLITTER_WEST_OFFSET,
    north: RC_SPLITTER_NORTH_OFFSET,
    south: RC_SPLITTER_SOUTH_OFFSET,
  })
  const bridgeUnderPole1 = renderer.addPole(
    BRIDGE_UNDER_POLE_1,
    FLOOR_CELL,
    BRIDGE_UNDER_POLE_1_OFFSET,
  )
  const bridgeUnderPole2 = renderer.addPole(
    BRIDGE_UNDER_POLE_2,
    FLOOR_CELL,
    BRIDGE_UNDER_POLE_2_OFFSET,
  )
  const rcTailMerger = renderer.addRouter(RC_TAIL_MERGER, FLOOR_CELL, {
    west: RC_TAIL_MERGER_WEST_OFFSET,
    north: RC_TAIL_MERGER_NORTH_OFFSET,
    east: RC_TAIL_MERGER_EAST_OFFSET,
  })
  const raSplitter = renderer.addRouter(RA_SPLITTER, FLOOR_CELL, {
    west: RA_SPLITTER_WEST_OFFSET,
    east: RA_SPLITTER_EAST_OFFSET,
    north: RA_SPLITTER_NORTH_OFFSET,
  })
  const overBridgePole = renderer.addPole(OVER_BRIDGE_POLE, FLOOR_CELL, OVER_BRIDGE_POLE_OFFSET)
  const mcSplitter = renderer.addRouter(MC_SPLITTER, FLOOR_CELL, {
    west: MC_SPLITTER_WEST_OFFSET,
    north: MC_SPLITTER_NORTH_OFFSET,
    east: MC_SPLITTER_EAST_OFFSET,
    south: MC_SPLITTER_SOUTH_OFFSET,
  })
  const mcTailMerger = renderer.addRouter(MC_TAIL_MERGER, FLOOR_CELL, {
    south: MC_TAIL_MERGER_SOUTH_OFFSET,
    east: MC_TAIL_MERGER_EAST_OFFSET,
    west: MC_TAIL_MERGER_WEST_OFFSET,
  })
  const mcFootMerger = renderer.addRouter(MC_FOOT_MERGER, FLOOR_CELL, {
    west: MC_FOOT_MERGER_WEST_OFFSET,
    north: MC_FOOT_MERGER_NORTH_OFFSET,
    east: MC_FOOT_MERGER_EAST_OFFSET,
  })
  const backToNcPole = renderer.addPole(BACK_TO_NC_POLE, FLOOR_CELL, BACK_TO_NC_POLE_OFFSET)
  const ncLoopMerger = renderer.addRouter(NC_LOOP_MERGER, FLOOR_CELL, {
    east: NC_LOOP_MERGER_EAST_OFFSET,
    south: NC_LOOP_MERGER_SOUTH_OFFSET,
    west: NC_LOOP_MERGER_WEST_OFFSET,
  })
  const overBridgeMerger = renderer.addRouter(OVER_BRIDGE_MERGER, FLOOR_CELL, {
    south: OVER_BRIDGE_MERGER_SOUTH_OFFSET,
    east: OVER_BRIDGE_MERGER_EAST_OFFSET,
    west: OVER_BRIDGE_MERGER_WEST_OFFSET,
  })
  const proPole1 = renderer.addPole(PRO_POLE_1, FLOOR_CELL, PRO_POLE1_OFFSET)
  const proPole2 = renderer.addPole(PRO_POLE_2, FLOOR_CELL, PRO_POLE2_OFFSET)
  const proPole3 = renderer.addPole(PRO_POLE_3, FLOOR_CELL, PRO_POLE3_OFFSET)
  const proPole4 = renderer.addPole(PRO_POLE_4, FLOOR_CELL, PRO_POLE4_OFFSET)

  // ─── Connecting belts ──────────────────────────────────────────
  // PR Opened.east → 4 corner poles → RFR.west.
  const beltPrOpenedToPole1 = renderer.addBelt(
    prOpened.ports[1], // PR Opened east output
    proPole1.ports.get('west')!,
    BELT_PRO_TO_POLE1,
    false,
    false,
  )
  const beltPole1ToPole2 = renderer.addBelt(
    proPole1.ports.get('south')!,
    proPole2.ports.get('north')!,
    BELT_POLE1_TO_POLE2,
    false,
    false,
  )
  const beltPole2ToPole3 = renderer.addBelt(
    proPole2.ports.get('west')!,
    proPole3.ports.get('east')!,
    BELT_POLE2_TO_POLE3,
    false,
    false,
  )
  const beltPole3ToPole4 = renderer.addBelt(
    proPole3.ports.get('south')!,
    proPole4.ports.get('north')!,
    BELT_POLE3_TO_POLE4,
    false,
    false,
  )
  const beltPole4ToRfr = renderer.addBelt(
    proPole4.ports.get('east')!,
    readyForReview.ports[0], // RFR west input
    BELT_POLE4_TO_RFR,
    false,
    false,
  )
  const beltRfrToIntakeSplitter = renderer.addBelt(
    readyForReview.ports[1], // RFR east output
    intakeSplitter.ports.get('west')!,
    BELT_RFR_TO_INTAKE_SPLITTER,
    false,
    false,
  )
  const beltIntakeSplitterToMerger = renderer.addBelt(
    intakeSplitter.ports.get('east')!,
    merger.ports.get('west')!,
    BELT_INTAKE_SPLITTER_TO_MERGER,
    false,
    false,
  )
  const beltMergerToNc = renderer.addBelt(
    merger.ports.get('east')!,
    newCommits.ports[0], // NC west input
    BELT_MERGER_TO_NC,
    false,
    false,
  )
  const beltNcToS1 = renderer.addBelt(
    newCommits.ports[1], // NC east output
    splitter1.ports.get('west')!,
    BELT_NC_TO_S1,
    false,
    false,
  )
  const beltS1ToS2 = renderer.addBelt(
    splitter1.ports.get('east')!,
    splitter2.ports.get('west')!,
    BELT_S1_TO_S2,
    false,
    false,
  )
  const beltS2ToCiPassed = renderer.addBelt(
    splitter2.ports.get('east')!,
    ciPassed.ports[0], // CI Passed west input
    BELT_S2_TO_CI_PASSED,
    false,
    false,
  )
  const beltCiPassedToPostCi = renderer.addBelt(
    ciPassed.ports[1], // CI Passed east output
    postCi.ports.get('west')!,
    BELT_CI_PASSED_TO_POST_CI,
    false,
    false,
  )
  // Above-S2 chain: S2.NORTH → pole_above.SOUTH → MC.
  const beltS2ToPoleAbove = renderer.addBelt(
    splitter2.ports.get('north')!,
    poleAbove.ports.get('south')!,
    BELT_S2_TO_POLE_ABOVE,
    false,
    false,
  )
  const beltPoleAboveToMc = renderer.addBelt(
    poleAbove.ports.get('east')!,
    mergeConflicts.ports[0],
    BELT_POLE_ABOVE_TO_MC,
    false,
    false,
  )
  // Below-S2 chain (mirror): S2.SOUTH → pole_below.NORTH → CI Failed.
  const beltS2ToPoleBelow = renderer.addBelt(
    splitter2.ports.get('south')!,
    poleBelow.ports.get('north')!,
    BELT_S2_TO_POLE_BELOW,
    false,
    false,
  )
  const beltPoleBelowToCiFailed = renderer.addBelt(
    poleBelow.ports.get('east')!,
    ciFailed.ports[0],
    BELT_POLE_BELOW_TO_CI_FAILED,
    false,
    false,
  )
  // Review chain: post-CI.east → review merger → RR → review splitter → 3 fan.
  const beltPostCiToRm = renderer.addBelt(
    postCi.ports.get('east')!,
    reviewMerger.ports.get('west')!,
    BELT_POSTCI_TO_RM,
    false,
    false,
  )
  const beltRmToRr = renderer.addBelt(
    reviewMerger.ports.get('east')!,
    reviewRequested.ports[0], // RR west input
    BELT_RM_TO_RR,
    false,
    false,
  )
  const beltRrToReview = renderer.addBelt(
    reviewRequested.ports[1], // RR east output
    reviewSplitter.ports.get('west')!,
    BELT_RR_TO_REVIEW,
    false,
    false,
  )
  const beltReviewToReviewCommented = renderer.addBelt(
    reviewSplitter.ports.get('east')!,
    reviewCommented.ports[0],
    BELT_REVIEW_TO_REVIEW_COMMENTED,
    false,
    false,
  )
  // Review-above chain: review.NORTH → pole.SOUTH → review_approved.
  const beltReviewToPoleReviewAbove = renderer.addBelt(
    reviewSplitter.ports.get('north')!,
    poleReviewAbove.ports.get('south')!,
    BELT_REVIEW_TO_POLE_REVIEW_ABOVE,
    false,
    false,
  )
  const beltPoleReviewAboveToRa = renderer.addBelt(
    poleReviewAbove.ports.get('east')!,
    reviewApproved.ports[0],
    BELT_POLE_REVIEW_ABOVE_TO_RA,
    false,
    false,
  )
  // Review-below chain (mirror): review.SOUTH → pole.NORTH → changes_requested.
  const beltReviewToPoleReviewBelow = renderer.addBelt(
    reviewSplitter.ports.get('south')!,
    poleReviewBelow.ports.get('north')!,
    BELT_REVIEW_TO_POLE_REVIEW_BELOW,
    false,
    false,
  )
  const beltPoleReviewBelowToCr = renderer.addBelt(
    poleReviewBelow.ports.get('east')!,
    changesRequested.ports[0],
    BELT_POLE_REVIEW_BELOW_TO_CR,
    false,
    false,
  )

  // CI Failed loopback: CIF.east + post-CI.south → loop_merger →
  // pole_2 → pole_3 → NC merger.south.
  const beltCifToLoopMerger = renderer.addBelt(
    ciFailed.ports[1], // CI Failed east output
    loopMerger.ports.get('west')!,
    BELT_CIF_TO_LOOP_MERGER,
    false,
    false,
  )
  const beltPostCiSouthToLoopMerger = renderer.addBelt(
    postCi.ports.get('south')!,
    loopMerger.ports.get('north')!,
    BELT_POSTCI_SOUTH_TO_LOOP_MERGER,
    false,
    false,
  )
  const beltLoopMergerToLoop2 = renderer.addBelt(
    loopMerger.ports.get('south')!,
    loopPole2.ports.get('north')!,
    BELT_LOOP_MERGER_TO_LOOP2,
    false,
    false,
  )
  const beltLoop2ToLoop3 = renderer.addBelt(
    loopPole2.ports.get('west')!,
    loopPole3.ports.get('east')!,
    BELT_LOOP2_TO_LOOP3,
    false,
    false,
  )
  const beltLoop3ToMerger = renderer.addBelt(
    loopPole3.ports.get('north')!,
    merger.ports.get('south')!,
    BELT_LOOP3_TO_MERGER,
    false,
    false,
  )
  // Ready-for-Review intake: intake_splitter.south → rfr_splitter
  // → rfr_pole_2 → RM.south. The rfr_splitter also branches a south
  // leg toward the new bottom_pole (row 8, eventually leading to
  // Closed). Both vertical legs sit clear of the loopback's
  // horizontal belt (row 12), so the chain doesn't cross any
  // existing belts.
  const beltIntakeSplitterSouthToRfrSplitter = renderer.addBelt(
    intakeSplitter.ports.get('south')!,
    rfrSplitter.ports.get('north')!,
    BELT_INTAKE_SPLITTER_SOUTH_TO_RFR_SPLITTER,
    false,
    false,
  )
  const beltRfrSplitterToRfrPole2 = renderer.addBelt(
    rfrSplitter.ports.get('east')!,
    rfrPole2.ports.get('west')!,
    BELT_RFR_SPLITTER_TO_RFR_POLE2,
    false,
    false,
  )
  const beltRfrPole2ToRm = renderer.addBelt(
    rfrPole2.ports.get('north')!,
    reviewMerger.ports.get('south')!,
    BELT_RFR_POLE2_TO_RM,
    false,
    false,
  )

  // Bottom chain: rfr_splitter.south → bottom_pole → row-8 belt
  // → bottom_merger.west. Bridge.south feeds bottom_merger.north
  // separately (zero-length connector at col 21).
  const beltRfrSplitterSouthToBottomPole = renderer.addBelt(
    rfrSplitter.ports.get('south')!,
    bottomPole.ports.get('north')!,
    BELT_RFR_SPLITTER_SOUTH_TO_BOTTOM_POLE,
    false,
    false,
  )
  const beltBottomPoleToBottomMerger = renderer.addBelt(
    bottomPole.ports.get('east')!,
    bottomMerger.ports.get('west')!,
    BELT_BOTTOM_POLE_TO_BOTTOM_MERGER,
    false,
    false,
  )
  const beltBottomMergerToMcFootMerger = renderer.addBelt(
    bottomMerger.ports.get('east')!,
    mcFootMerger.ports.get('west')!,
    BELT_BOTTOM_MERGER_TO_MC_FOOT_MERGER,
    false,
    false,
  )
  const beltMcFootMergerToCrMerger = renderer.addBelt(
    mcFootMerger.ports.get('east')!,
    crMerger.ports.get('west')!,
    BELT_MC_FOOT_MERGER_TO_CR_MERGER,
    false,
    false,
  )
  const beltCrMergerToRcTailMerger = renderer.addBelt(
    crMerger.ports.get('east')!,
    rcTailMerger.ports.get('west')!,
    BELT_CR_MERGER_TO_RC_TAIL_MERGER,
    false,
    false,
  )
  const beltRcTailMergerToClosed = renderer.addBelt(
    rcTailMerger.ports.get('east')!,
    closed.ports[0], // Closed west input
    BELT_RC_TAIL_MERGER_TO_CLOSED,
    false,
    false,
  )

  // Changes Requested → CR_SPLITTER → CR_MERGER.north. CR's east
  // port wakes up here (was a dead-end terminal); the splitter then
  // drops a south leg straight down 6 cells into CR_MERGER and a
  // north leg up 3 cells into the new RC_MERGER on the row-19 line.
  const beltCrToCrSplitter = renderer.addBelt(
    changesRequested.ports[1], // CR east output
    crSplitter.ports.get('west')!,
    BELT_CR_TO_CR_SPLITTER,
    false,
    false,
  )
  const beltCrSplitterSouthToCrMerger = renderer.addBelt(
    crSplitter.ports.get('south')!,
    crMerger.ports.get('north')!,
    BELT_CR_SPLITTER_SOUTH_TO_CR_MERGER,
    false,
    false,
  )
  const beltCrSplitterNorthToRcMerger = renderer.addBelt(
    crSplitter.ports.get('north')!,
    rcMerger.ports.get('south')!,
    BELT_CR_SPLITTER_NORTH_TO_RC_MERGER,
    false,
    false,
  )

  // Review Commented → RC_MERGER → 1-cell belt → RC_POLE. RC was a
  // terminal until now; the merger also receives CR_SPLITTER's
  // north output (handled above). The pole's north output is
  // dormant until a future destination lands.
  const beltRcToRcMerger = renderer.addBelt(
    reviewCommented.ports[1], // RC east output
    rcMerger.ports.get('west')!,
    BELT_RC_TO_RC_MERGER,
    false,
    false,
  )
  const beltRcMergerToRcSplitter = renderer.addBelt(
    rcMerger.ports.get('east')!,
    rcSplitter.ports.get('west')!,
    BELT_RC_MERGER_TO_RC_SPLITTER,
    false,
    false,
  )

  // Under-bridge route: RC_SPLITTER.north → 2-cell vertical →
  // BRIDGE_UNDER_POLE_1 (turn west) → 2-cell horizontal → POLE_2
  // (turn south) → dead-end stub.
  const beltRcSplitterNorthToUnderPole1 = renderer.addBelt(
    rcSplitter.ports.get('north')!,
    bridgeUnderPole1.ports.get('south')!,
    BELT_RC_SPLITTER_NORTH_TO_UNDER_POLE_1,
    false,
    false,
  )
  const beltUnderPole1ToUnderPole2 = renderer.addBelt(
    bridgeUnderPole1.ports.get('west')!,
    bridgeUnderPole2.ports.get('east')!,
    BELT_UNDER_POLE_1_TO_UNDER_POLE_2,
    false,
    false,
  )
  const beltUnderPole2ToOverBridgeMerger = renderer.addBelt(
    bridgeUnderPole2.ports.get('north')!,
    overBridgeMerger.ports.get('south')!,
    BELT_UNDER_POLE_2_TO_OVER_BRIDGE_MERGER,
    false,
    false,
  )

  // South branch: RC_SPLITTER.south → 10-cell vertical → RC_TAIL_MERGER.north.
  const beltRcSplitterSouthToRcTailMerger = renderer.addBelt(
    rcSplitter.ports.get('south')!,
    rcTailMerger.ports.get('north')!,
    BELT_RC_SPLITTER_SOUTH_TO_RC_TAIL_MERGER,
    false,
    false,
  )

  // Review Approved → Merged: a compact 3-cell east-flowing bridge
  // (1 ramp + 1 flat + 1 ramp) starts at RA.east. A 2-cell flat
  // connector belt then carries the chip the rest of the way to
  // Merged.west at floor level. Chevron jump (if any) lands at
  // Merged.west; the connector belt's pathOffset picks up the
  // bridge's actual arc-length end UV at runtime.
  const raBridgeY = (REVIEW_APPROVED_ROW + 1.5) * FLOOR_CELL
  const raBridgeStartX = (REVIEW_DEST_COL + STATION_W) * FLOOR_CELL
  const raBridgeEndX = raBridgeStartX + RA_BRIDGE_CELL_COUNT * FLOOR_CELL
  const raBridge = renderer.addBridge({
    start: new Vector3(raBridgeStartX, raBridgeY, 0),
    end: new Vector3(raBridgeEndX, raBridgeY, 0),
    cellCount: RA_BRIDGE_CELL_COUNT,
    peakHeight: BRIDGE_PEAK_HEIGHT,
    pathOffset: BELT_RA_BRIDGE_PATH_OFFSET,
  })
  // Bridge butts directly against RA_SPLITTER's west wall (zero-
  // length connector — they share the same xy point at floor-z).
  // Splitter east → 1-cell flat belt → Merged.west.
  const beltRaSplitterToMerged = renderer.addBelt(
    raSplitter.ports.get('east')!,
    merged.ports[0],
    BELT_RA_SPLITTER_TO_MERGED,
    false,
    false,
  )
  // Splitter.north → 2-cell vertical drop → over-bridge pole →
  // 1-cell horizontal → over-bridge merger.east. The "needs more
  // commits" branch — placeholder route until the eventual loop
  // back to the new-commits chain lands.
  const beltRaSplitterNorthToOverBridgePole = renderer.addBelt(
    raSplitter.ports.get('north')!,
    overBridgePole.ports.get('south')!,
    BELT_RA_SPLITTER_NORTH_TO_OVER_BRIDGE_POLE,
    false,
    false,
  )
  const beltOverBridgePoleToOverBridgeMerger = renderer.addBelt(
    overBridgePole.ports.get('west')!,
    overBridgeMerger.ports.get('east')!,
    BELT_OVER_BRIDGE_POLE_TO_OVER_BRIDGE_MERGER,
    false,
    false,
  )

  // Merge Conflicts → MC_SPLITTER → 2-cell vertical → MC_TAIL_MERGER.
  // MC was a true terminal until now. East/south splitter outputs
  // stay dead-end stubs (reserved for the future MC → Closed path).
  const beltMcToMcSplitter = renderer.addBelt(
    mergeConflicts.ports[1], // MC east output
    mcSplitter.ports.get('west')!,
    BELT_MC_TO_MC_SPLITTER,
    false,
    false,
  )
  const beltMcSplitterNorthToMcTailMerger = renderer.addBelt(
    mcSplitter.ports.get('north')!,
    mcTailMerger.ports.get('south')!,
    BELT_MC_SPLITTER_NORTH_TO_MC_TAIL_MERGER,
    false,
    false,
  )
  // OVER_BRIDGE_MERGER → MC_TAIL_MERGER: long row-26 belt that
  // pulls the merged CR/RC/RA stream westward into the MC merger,
  // joining the back-to-NC path.
  const beltOverBridgeMergerToMcTailMerger = renderer.addBelt(
    overBridgeMerger.ports.get('west')!,
    mcTailMerger.ports.get('east')!,
    BELT_OVER_BRIDGE_MERGER_TO_MC_TAIL_MERGER,
    false,
    false,
  )

  // Row-26 back-to-NC belt, split into two halves by NC_LOOP_MERGER.
  // East half: MC_TAIL_MERGER.west → 11-cell belt → NC_LOOP_MERGER.east.
  // West half: NC_LOOP_MERGER.west → 7-cell belt → BACK_TO_NC_POLE.east.
  // Pole.south → 6-cell drop → NC merger.north (closes the loop).
  const beltMcTailMergerToNcLoopMerger = renderer.addBelt(
    mcTailMerger.ports.get('west')!,
    ncLoopMerger.ports.get('east')!,
    BELT_MC_TAIL_MERGER_TO_NC_LOOP_MERGER,
    false,
    false,
  )
  const beltNcLoopMergerToBackToNcPole = renderer.addBelt(
    ncLoopMerger.ports.get('west')!,
    backToNcPole.ports.get('east')!,
    BELT_NC_LOOP_MERGER_TO_BACK_TO_NC_POLE,
    false,
    false,
  )
  const beltBackToNcPoleToMerger = renderer.addBelt(
    backToNcPole.ports.get('south')!,
    merger.ports.get('north')!,
    BELT_BACK_TO_NC_POLE_TO_MERGER,
    false,
    false,
  )

  // S1.north → 6-cell vertical climb at col 21 → NC_LOOP_MERGER.south.
  // The "NC commits beget more NC commits" short loop.
  const beltS1NorthToNcLoopMerger = renderer.addBelt(
    splitter1.ports.get('north')!,
    ncLoopMerger.ports.get('south')!,
    BELT_S1_NORTH_TO_NC_LOOP_MERGER,
    false,
    false,
  )

  // MC_SPLITTER south branch: 14-cell descent at col 33 into
  // MC_FOOT_MERGER on the row-8 Closed line. Two arched bridges
  // hop the POST_CI→RM belt at row 19 and the RFR-intake belt at
  // row 11 (each 3 cells, peak directly over the crossing). Three
  // flat belts fill the gaps. Forward-anchor chevrons through the
  // chain at runtime since each bridge's true arc length depends
  // on the ramp curvature.
  const mcChainX = (MC_SPLITTER_COL + 0.5) * FLOOR_CELL
  const mcSplitterSouthY = MC_SPLITTER_ROW * FLOOR_CELL
  const mcBridge1NorthY = (MC_BRIDGE_1_NORTH_ROW + 1) * FLOOR_CELL
  const mcBridge1SouthY = (MC_BRIDGE_1_NORTH_ROW + 1 - MC_BRIDGE_CELL_COUNT) * FLOOR_CELL
  const mcBridge2NorthY = (MC_BRIDGE_2_NORTH_ROW + 1) * FLOOR_CELL
  const mcBridge2SouthY = (MC_BRIDGE_2_NORTH_ROW + 1 - MC_BRIDGE_CELL_COUNT) * FLOOR_CELL
  const mcFootMergerNorthY = (MC_FOOT_MERGER_ROW + 1) * FLOOR_CELL
  const mcFlat1Len = mcSplitterSouthY - mcBridge1NorthY // 160
  const mcFlat2Len = mcBridge1SouthY - mcBridge2NorthY // 400
  // mcFlat3 (1 cell, 80 wu) is the trailing belt — its pathOffset
  // is computed from the cumulative chain UV but its length itself
  // doesn't enter further math (the chevron jump lands at the
  // merger's north port frame).
  const mcSplitterSouthWallUV = mod(MC_SPLITTER_SOUTH_OFFSET + ROUTER_STUB_LEN)

  const beltMcFlat1 = renderer.addBeltAt(
    new Vector3(mcChainX, mcSplitterSouthY, 0),
    new Vector3(mcChainX, mcBridge1NorthY, 0),
    mcSplitterSouthWallUV,
    false,
    false,
  )
  const mcBridge1 = renderer.addBridge({
    start: new Vector3(mcChainX, mcBridge1NorthY, 0),
    end: new Vector3(mcChainX, mcBridge1SouthY, 0),
    cellCount: MC_BRIDGE_CELL_COUNT,
    peakHeight: BRIDGE_PEAK_HEIGHT,
    pathOffset: mod(mcSplitterSouthWallUV + mcFlat1Len),
  })
  const beltMcFlat2 = renderer.addBeltAt(
    new Vector3(mcChainX, mcBridge1SouthY, 0),
    new Vector3(mcChainX, mcBridge2NorthY, 0),
    mod(mcSplitterSouthWallUV + mcFlat1Len + mcBridge1.pathLength),
    false,
    false,
  )
  const mcBridge2 = renderer.addBridge({
    start: new Vector3(mcChainX, mcBridge2NorthY, 0),
    end: new Vector3(mcChainX, mcBridge2SouthY, 0),
    cellCount: MC_BRIDGE_CELL_COUNT,
    peakHeight: BRIDGE_PEAK_HEIGHT,
    pathOffset: mod(mcSplitterSouthWallUV + mcFlat1Len + mcBridge1.pathLength + mcFlat2Len),
  })
  const beltMcFlat3 = renderer.addBeltAt(
    new Vector3(mcChainX, mcBridge2SouthY, 0),
    new Vector3(mcChainX, mcFootMergerNorthY, 0),
    mod(
      mcSplitterSouthWallUV + mcFlat1Len + mcBridge1.pathLength + mcFlat2Len + mcBridge2.pathLength,
    ),
    false,
    false,
  )

  // ─── Bridge ────────────────────────────────────────────────────
  // S1.south → connector belt → flat-top overpass crossing the row-11
  // RFR intake and the row-12 CI Failed loopback. Bridge's south end
  // is a dead-end for now (items entering get despawned); the south
  // exit will be wired into a downstream destination later.
  const bridge = renderer.addBridge({
    start: new Vector3((BRIDGE_COL + 0.5) * FLOOR_CELL, (BRIDGE_NORTH_ROW + 1) * FLOOR_CELL, 0),
    end: new Vector3(
      (BRIDGE_COL + 0.5) * FLOOR_CELL,
      (BRIDGE_NORTH_ROW + 1 - BRIDGE_CELL_COUNT) * FLOOR_CELL,
      0,
    ),
    cellCount: BRIDGE_CELL_COUNT,
    peakHeight: BRIDGE_PEAK_HEIGHT,
    pathOffset: BRIDGE_PATH_OFFSET,
  })
  const s1SouthHandle = splitter1.ports.get('south')!
  const beltS1SouthToBridge = renderer.addBeltAt(
    new Vector3(s1SouthHandle.worldPos.x, s1SouthHandle.worldPos.y, 0),
    new Vector3((BRIDGE_COL + 0.5) * FLOOR_CELL, (BRIDGE_NORTH_ROW + 1) * FLOOR_CELL, 0),
    BELT_S1_SOUTH_TO_BRIDGE,
    false,
    false,
  )

  // ─── Item path graph ──────────────────────────────────────────
  // Spawn at intake splitter's west input. Every station the items
  // must transit (NC, CI Passed, CI Failed) is wired passthrough
  // so chips don't dead-end at every recess. MC remains a true
  // terminal — its west input has no `.next`, so chips entering
  // it despawn at the recess back.

  // Intake splitter: west input fans to east (→ merger) and south
  // (→ ready-for-review intake chain). Simulator picks next[0] =
  // east, so today every chip continues into the NC merger.
  // PR Opened (passthrough) → 4 corner poles → RFR (passthrough)
  // → belt → intake splitter.
  prOpened.ports[0].segment!.next = [prOpened.ports[1].segment!]
  prOpened.ports[1].segment!.next = [beltPrOpenedToPole1.segment]
  beltPrOpenedToPole1.segment.next = [proPole1.internalSegment]
  proPole1.internalSegment.next = [beltPole1ToPole2.segment]
  beltPole1ToPole2.segment.next = [proPole2.internalSegment]
  proPole2.internalSegment.next = [beltPole2ToPole3.segment]
  beltPole2ToPole3.segment.next = [proPole3.internalSegment]
  proPole3.internalSegment.next = [beltPole3ToPole4.segment]
  beltPole3ToPole4.segment.next = [proPole4.internalSegment]
  proPole4.internalSegment.next = [beltPole4ToRfr.segment]
  beltPole4ToRfr.segment.next = [readyForReview.ports[0].segment!]
  readyForReview.ports[0].segment!.next = [readyForReview.ports[1].segment!]
  readyForReview.ports[1].segment!.next = [beltRfrToIntakeSplitter.segment]
  beltRfrToIntakeSplitter.segment.next = [intakeSplitter.ports.get('west')!.segment!]
  intakeSplitter.ports.get('west')!.segment!.next = [
    intakeSplitter.ports.get('east')!.segment!,
    intakeSplitter.ports.get('south')!.segment!,
  ]
  intakeSplitter.ports.get('east')!.segment!.next = [beltIntakeSplitterToMerger.segment]
  beltIntakeSplitterToMerger.segment.next = [merger.ports.get('west')!.segment!]

  // Merger: both inputs converge to east output.
  merger.ports.get('west')!.segment!.next = [merger.ports.get('east')!.segment!]
  merger.ports.get('south')!.segment!.next = [merger.ports.get('east')!.segment!]
  merger.ports.get('east')!.segment!.next = [beltMergerToNc.segment]

  // intake_splitter.south → rfr_splitter (fans east + south).
  // east → rfr_pole_2 → RM.south (existing review-intake chain).
  // south → bottom_pole → bottom_merger.west → bottom_merger.east
  //   (dead-end, future Closed). Bridge.south joins via merger.north.
  intakeSplitter.ports.get('south')!.segment!.next = [beltIntakeSplitterSouthToRfrSplitter.segment]
  beltIntakeSplitterSouthToRfrSplitter.segment.next = [rfrSplitter.ports.get('north')!.segment!]
  rfrSplitter.ports.get('north')!.segment!.next = [
    rfrSplitter.ports.get('east')!.segment!,
    rfrSplitter.ports.get('south')!.segment!,
  ]
  rfrSplitter.ports.get('east')!.segment!.next = [beltRfrSplitterToRfrPole2.segment]
  beltRfrSplitterToRfrPole2.segment.next = [rfrPole2.internalSegment]
  rfrPole2.internalSegment.next = [beltRfrPole2ToRm.segment]
  beltRfrPole2ToRm.segment.next = [reviewMerger.ports.get('south')!.segment!]
  rfrSplitter.ports.get('south')!.segment!.next = [beltRfrSplitterSouthToBottomPole.segment]
  beltRfrSplitterSouthToBottomPole.segment.next = [bottomPole.internalSegment]
  bottomPole.internalSegment.next = [beltBottomPoleToBottomMerger.segment]
  beltBottomPoleToBottomMerger.segment.next = [bottomMerger.ports.get('west')!.segment!]
  bottomMerger.ports.get('west')!.segment!.next = [bottomMerger.ports.get('east')!.segment!]
  bottomMerger.ports.get('north')!.segment!.next = [bottomMerger.ports.get('east')!.segment!]
  bottomMerger.ports.get('east')!.segment!.next = [beltBottomMergerToMcFootMerger.segment]
  beltBottomMergerToMcFootMerger.segment.next = [mcFootMerger.ports.get('west')!.segment!]
  mcFootMerger.ports.get('west')!.segment!.next = [mcFootMerger.ports.get('east')!.segment!]
  mcFootMerger.ports.get('north')!.segment!.next = [mcFootMerger.ports.get('east')!.segment!]
  mcFootMerger.ports.get('east')!.segment!.next = [beltMcFootMergerToCrMerger.segment]
  beltMcFootMergerToCrMerger.segment.next = [crMerger.ports.get('west')!.segment!]
  crMerger.ports.get('west')!.segment!.next = [crMerger.ports.get('east')!.segment!]
  crMerger.ports.get('north')!.segment!.next = [crMerger.ports.get('east')!.segment!]
  crMerger.ports.get('east')!.segment!.next = [beltCrMergerToRcTailMerger.segment]
  beltCrMergerToRcTailMerger.segment.next = [rcTailMerger.ports.get('west')!.segment!]
  rcTailMerger.ports.get('west')!.segment!.next = [rcTailMerger.ports.get('east')!.segment!]
  rcTailMerger.ports.get('north')!.segment!.next = [rcTailMerger.ports.get('east')!.segment!]
  rcTailMerger.ports.get('east')!.segment!.next = [beltRcTailMergerToClosed.segment]
  beltRcTailMergerToClosed.segment.next = [closed.ports[0].segment!]
  // closed.west has no .next — terminal.

  // Merger → NC (passthrough) → S1.
  beltMergerToNc.segment.next = [newCommits.ports[0].segment!]
  newCommits.ports[0].segment!.next = [newCommits.ports[1].segment!]
  newCommits.ports[1].segment!.next = [beltNcToS1.segment]
  beltNcToS1.segment.next = [splitter1.ports.get('west')!.segment!]

  // S1 fans east (default → S2), south (bridge → Closed loop), and
  // north (NC→NC short loop). Simulator picks index 0 = east.
  splitter1.ports.get('west')!.segment!.next = [
    splitter1.ports.get('east')!.segment!,
    splitter1.ports.get('south')!.segment!,
    splitter1.ports.get('north')!.segment!,
  ]
  splitter1.ports.get('east')!.segment!.next = [beltS1ToS2.segment]
  // S1.south → connector belt → bridge → bottom_merger.north. The
  // bridge's south end meets the merger's north wall directly (no
  // connector), so items continue cleanly through the merger to its
  // east output (currently dead-ended; future Closed). Items only
  // reach this branch if S1's west fan picks next[1]; today next[0]
  // is east, so the bridge sits dormant until a routing-decision
  // layer flips the order.
  splitter1.ports.get('south')!.segment!.next = [beltS1SouthToBridge.segment]
  beltS1SouthToBridge.segment.next = [bridge.segment]
  bridge.segment.next = [bottomMerger.ports.get('north')!.segment!]

  // S1 → S2.
  beltS1ToS2.segment.next = [splitter2.ports.get('west')!.segment!]
  splitter2.ports.get('west')!.segment!.next = [
    splitter2.ports.get('east')!.segment!,
    splitter2.ports.get('north')!.segment!,
    splitter2.ports.get('south')!.segment!,
  ]

  // S2.east → CI Passed (passthrough) → post-CI.
  splitter2.ports.get('east')!.segment!.next = [beltS2ToCiPassed.segment]
  beltS2ToCiPassed.segment.next = [ciPassed.ports[0].segment!]
  ciPassed.ports[0].segment!.next = [ciPassed.ports[1].segment!]
  ciPassed.ports[1].segment!.next = [beltCiPassedToPostCi.segment]
  beltCiPassedToPostCi.segment.next = [postCi.ports.get('west')!.segment!]
  postCi.ports.get('west')!.segment!.next = [
    postCi.ports.get('east')!.segment!,
    postCi.ports.get('north')!.segment!,
    postCi.ports.get('south')!.segment!,
  ]
  // Post-CI: east extends into the review chain below; south
  // feeds back into the loop merger (wired in the loopback
  // section); north stays dead-ended (staged for future wiring).

  // post-CI.east → review merger → RR (passthrough) → review splitter.
  postCi.ports.get('east')!.segment!.next = [beltPostCiToRm.segment]
  beltPostCiToRm.segment.next = [reviewMerger.ports.get('west')!.segment!]
  reviewMerger.ports.get('west')!.segment!.next = [reviewMerger.ports.get('east')!.segment!]
  // reviewMerger.south now receives the ready-for-review intake
  // chain. It will eventually also receive the changes_requested
  // loopback (multiple inputs → single east output).
  reviewMerger.ports.get('south')!.segment!.next = [reviewMerger.ports.get('east')!.segment!]
  reviewMerger.ports.get('east')!.segment!.next = [beltRmToRr.segment]
  beltRmToRr.segment.next = [reviewRequested.ports[0].segment!]
  reviewRequested.ports[0].segment!.next = [reviewRequested.ports[1].segment!]
  reviewRequested.ports[1].segment!.next = [beltRrToReview.segment]
  beltRrToReview.segment.next = [reviewSplitter.ports.get('west')!.segment!]
  reviewSplitter.ports.get('west')!.segment!.next = [
    reviewSplitter.ports.get('east')!.segment!,
    reviewSplitter.ports.get('north')!.segment!,
    reviewSplitter.ports.get('south')!.segment!,
  ]

  // review.east → review_commented (passthrough) → rc_merger →
  // rc_splitter, which fans north (under-bridge route, dead-ends
  // at bridge_under_pole_2.south) and south (drops to rc_tail_merger
  // on the row-8 Closed line).
  reviewSplitter.ports.get('east')!.segment!.next = [beltReviewToReviewCommented.segment]
  beltReviewToReviewCommented.segment.next = [reviewCommented.ports[0].segment!]
  reviewCommented.ports[0].segment!.next = [reviewCommented.ports[1].segment!]
  reviewCommented.ports[1].segment!.next = [beltRcToRcMerger.segment]
  beltRcToRcMerger.segment.next = [rcMerger.ports.get('west')!.segment!]
  rcMerger.ports.get('west')!.segment!.next = [rcMerger.ports.get('east')!.segment!]
  rcMerger.ports.get('south')!.segment!.next = [rcMerger.ports.get('east')!.segment!]
  rcMerger.ports.get('east')!.segment!.next = [beltRcMergerToRcSplitter.segment]
  beltRcMergerToRcSplitter.segment.next = [rcSplitter.ports.get('west')!.segment!]
  rcSplitter.ports.get('west')!.segment!.next = [
    rcSplitter.ports.get('north')!.segment!,
    rcSplitter.ports.get('south')!.segment!,
  ]
  // North branch: under-bridge route, climbs to OVER_BRIDGE_MERGER.
  // The merger's west output is dormant for now; east input is a
  // placeholder dead-end stub for a future feed.
  rcSplitter.ports.get('north')!.segment!.next = [beltRcSplitterNorthToUnderPole1.segment]
  beltRcSplitterNorthToUnderPole1.segment.next = [bridgeUnderPole1.internalSegment]
  bridgeUnderPole1.internalSegment.next = [beltUnderPole1ToUnderPole2.segment]
  beltUnderPole1ToUnderPole2.segment.next = [bridgeUnderPole2.internalSegment]
  bridgeUnderPole2.internalSegment.next = [beltUnderPole2ToOverBridgeMerger.segment]
  beltUnderPole2ToOverBridgeMerger.segment.next = [overBridgeMerger.ports.get('south')!.segment!]
  overBridgeMerger.ports.get('south')!.segment!.next = [
    overBridgeMerger.ports.get('west')!.segment!,
  ]
  overBridgeMerger.ports.get('east')!.segment!.next = [overBridgeMerger.ports.get('west')!.segment!]
  overBridgeMerger.ports.get('west')!.segment!.next = [beltOverBridgeMergerToMcTailMerger.segment]
  beltOverBridgeMergerToMcTailMerger.segment.next = [mcTailMerger.ports.get('east')!.segment!]
  // South branch: drop to rc_tail_merger.north → Closed.
  rcSplitter.ports.get('south')!.segment!.next = [beltRcSplitterSouthToRcTailMerger.segment]
  beltRcSplitterSouthToRcTailMerger.segment.next = [rcTailMerger.ports.get('north')!.segment!]

  // review.north → pole_review_above → review_approved (passthrough)
  // → ra_bridge → Merged (terminal).
  reviewSplitter.ports.get('north')!.segment!.next = [beltReviewToPoleReviewAbove.segment]
  beltReviewToPoleReviewAbove.segment.next = [poleReviewAbove.internalSegment]
  poleReviewAbove.internalSegment.next = [beltPoleReviewAboveToRa.segment]
  beltPoleReviewAboveToRa.segment.next = [reviewApproved.ports[0].segment!]
  reviewApproved.ports[0].segment!.next = [reviewApproved.ports[1].segment!]
  reviewApproved.ports[1].segment!.next = [raBridge.segment]
  raBridge.segment.next = [raSplitter.ports.get('west')!.segment!]
  raSplitter.ports.get('west')!.segment!.next = [
    raSplitter.ports.get('east')!.segment!,
    raSplitter.ports.get('north')!.segment!,
  ]
  raSplitter.ports.get('east')!.segment!.next = [beltRaSplitterToMerged.segment]
  beltRaSplitterToMerged.segment.next = [merged.ports[0].segment!]
  // merged.west has no .next — terminal.
  raSplitter.ports.get('north')!.segment!.next = [beltRaSplitterNorthToOverBridgePole.segment]
  beltRaSplitterNorthToOverBridgePole.segment.next = [overBridgePole.internalSegment]
  overBridgePole.internalSegment.next = [beltOverBridgePoleToOverBridgeMerger.segment]
  beltOverBridgePoleToOverBridgeMerger.segment.next = [overBridgeMerger.ports.get('east')!.segment!]

  // review.south → pole_review_below → changes_requested
  // (passthrough) → cr_splitter → cr_merger → Closed.
  reviewSplitter.ports.get('south')!.segment!.next = [beltReviewToPoleReviewBelow.segment]
  beltReviewToPoleReviewBelow.segment.next = [poleReviewBelow.internalSegment]
  poleReviewBelow.internalSegment.next = [beltPoleReviewBelowToCr.segment]
  beltPoleReviewBelowToCr.segment.next = [changesRequested.ports[0].segment!]
  changesRequested.ports[0].segment!.next = [changesRequested.ports[1].segment!]
  changesRequested.ports[1].segment!.next = [beltCrToCrSplitter.segment]
  beltCrToCrSplitter.segment.next = [crSplitter.ports.get('west')!.segment!]
  crSplitter.ports.get('west')!.segment!.next = [
    crSplitter.ports.get('east')!.segment!,
    crSplitter.ports.get('south')!.segment!,
    crSplitter.ports.get('north')!.segment!,
  ]
  crSplitter.ports.get('south')!.segment!.next = [beltCrSplitterSouthToCrMerger.segment]
  beltCrSplitterSouthToCrMerger.segment.next = [crMerger.ports.get('north')!.segment!]
  crSplitter.ports.get('north')!.segment!.next = [beltCrSplitterNorthToRcMerger.segment]
  beltCrSplitterNorthToRcMerger.segment.next = [rcMerger.ports.get('south')!.segment!]
  // crSplitter.east stays a dead-end stub for a future destination.

  // S2.north → pole_above → MC (terminal).
  splitter2.ports.get('north')!.segment!.next = [beltS2ToPoleAbove.segment]
  beltS2ToPoleAbove.segment.next = [poleAbove.internalSegment]
  poleAbove.internalSegment.next = [beltPoleAboveToMc.segment]
  beltPoleAboveToMc.segment.next = [mergeConflicts.ports[0].segment!]
  // MC was a terminal — now passthrough to mc_splitter, which fans
  // north (active, toward MC_TAIL_MERGER on the back-to-NC path) +
  // east + south (dead-end stubs reserved for the future Closed
  // connection).
  mergeConflicts.ports[0].segment!.next = [mergeConflicts.ports[1].segment!]
  mergeConflicts.ports[1].segment!.next = [beltMcToMcSplitter.segment]
  beltMcToMcSplitter.segment.next = [mcSplitter.ports.get('west')!.segment!]
  mcSplitter.ports.get('west')!.segment!.next = [
    mcSplitter.ports.get('north')!.segment!,
    mcSplitter.ports.get('east')!.segment!,
    mcSplitter.ports.get('south')!.segment!,
  ]
  mcSplitter.ports.get('north')!.segment!.next = [beltMcSplitterNorthToMcTailMerger.segment]
  beltMcSplitterNorthToMcTailMerger.segment.next = [mcTailMerger.ports.get('south')!.segment!]
  mcTailMerger.ports.get('south')!.segment!.next = [mcTailMerger.ports.get('west')!.segment!]
  mcTailMerger.ports.get('east')!.segment!.next = [mcTailMerger.ports.get('west')!.segment!]
  // mcTailMerger.west → 11-cell belt → NC_LOOP_MERGER.east →
  // 7-cell belt → back-to-NC pole → 6-cell drop → NC merger.north.
  // The NC loop merger also catches S1.north (NC→NC short loop).
  mcTailMerger.ports.get('west')!.segment!.next = [beltMcTailMergerToNcLoopMerger.segment]
  beltMcTailMergerToNcLoopMerger.segment.next = [ncLoopMerger.ports.get('east')!.segment!]
  ncLoopMerger.ports.get('east')!.segment!.next = [ncLoopMerger.ports.get('west')!.segment!]
  ncLoopMerger.ports.get('south')!.segment!.next = [ncLoopMerger.ports.get('west')!.segment!]
  ncLoopMerger.ports.get('west')!.segment!.next = [beltNcLoopMergerToBackToNcPole.segment]
  beltNcLoopMergerToBackToNcPole.segment.next = [backToNcPole.internalSegment]
  backToNcPole.internalSegment.next = [beltBackToNcPoleToMerger.segment]
  beltBackToNcPoleToMerger.segment.next = [merger.ports.get('north')!.segment!]
  merger.ports.get('north')!.segment!.next = [merger.ports.get('east')!.segment!]
  // S1.north → vertical climb → NC_LOOP_MERGER.south. (S1 fan
  // updated below to include the north branch.)
  splitter1.ports.get('north')!.segment!.next = [beltS1NorthToNcLoopMerger.segment]
  beltS1NorthToNcLoopMerger.segment.next = [ncLoopMerger.ports.get('south')!.segment!]

  // MC_SPLITTER south branch: descends through 2 bridges to
  // MC_FOOT_MERGER on the row-8 Closed line.
  mcSplitter.ports.get('south')!.segment!.next = [beltMcFlat1.segment]
  beltMcFlat1.segment.next = [mcBridge1.segment]
  mcBridge1.segment.next = [beltMcFlat2.segment]
  beltMcFlat2.segment.next = [mcBridge2.segment]
  mcBridge2.segment.next = [beltMcFlat3.segment]
  beltMcFlat3.segment.next = [mcFootMerger.ports.get('north')!.segment!]

  // S2.south → pole_below → CI Failed (passthrough) → loopback.
  splitter2.ports.get('south')!.segment!.next = [beltS2ToPoleBelow.segment]
  beltS2ToPoleBelow.segment.next = [poleBelow.internalSegment]
  poleBelow.internalSegment.next = [beltPoleBelowToCiFailed.segment]
  beltPoleBelowToCiFailed.segment.next = [ciFailed.ports[0].segment!]
  ciFailed.ports[0].segment!.next = [ciFailed.ports[1].segment!]
  ciFailed.ports[1].segment!.next = [beltCifToLoopMerger.segment]
  beltCifToLoopMerger.segment.next = [loopMerger.ports.get('west')!.segment!]
  // Post-CI south output now also feeds the loop merger (the
  // "people commit even when CI passes" branch). Both inputs
  // converge to the merger's south output.
  postCi.ports.get('south')!.segment!.next = [beltPostCiSouthToLoopMerger.segment]
  beltPostCiSouthToLoopMerger.segment.next = [loopMerger.ports.get('north')!.segment!]
  loopMerger.ports.get('west')!.segment!.next = [loopMerger.ports.get('south')!.segment!]
  loopMerger.ports.get('north')!.segment!.next = [loopMerger.ports.get('south')!.segment!]
  loopMerger.ports.get('south')!.segment!.next = [beltLoopMergerToLoop2.segment]
  beltLoopMergerToLoop2.segment.next = [loopPole2.internalSegment]
  loopPole2.internalSegment.next = [beltLoop2ToLoop3.segment]
  beltLoop2ToLoop3.segment.next = [loopPole3.internalSegment]
  loopPole3.internalSegment.next = [beltLoop3ToMerger.segment]
  beltLoop3ToMerger.segment.next = [merger.ports.get('south')!.segment!]

  // ─── Per-station registry ─────────────────────────────────────
  // One row per station — keyed by event_type (= the station's id).
  // Pairs the spec/label with the live mesh handle (for setQueuedCount
  // / setRunCount) and the latest ClickedStationInfo to replay when
  // the user clicks. The reconciler below mutates `data` in place
  // each frame; click callback returns whatever's most recent.
  type StationRow = {
    spec: Station
    handle: ReturnType<IsoScene['addStation']>
    data: ClickedStationInfo
  }
  const stationRows: { spec: Station; handle: ReturnType<IsoScene['addStation']> }[] = [
    { spec: PR_OPENED, handle: prOpened },
    { spec: READY_FOR_REVIEW, handle: readyForReview },
    { spec: NEW_COMMITS, handle: newCommits },
    { spec: MERGE_CONFLICTS, handle: mergeConflicts },
    { spec: CI_PASSED, handle: ciPassed },
    { spec: CI_FAILED, handle: ciFailed },
    { spec: REVIEW_REQUESTED, handle: reviewRequested },
    { spec: REVIEW_APPROVED, handle: reviewApproved },
    { spec: REVIEW_COMMENTED, handle: reviewCommented },
    { spec: CHANGES_REQUESTED, handle: changesRequested },
    { spec: CLOSED, handle: closed },
    { spec: MERGED, handle: merged },
  ]
  const stationsByEvent = new Map<string, StationRow>()
  for (const { spec, handle } of stationRows) {
    if (!spec.id) continue
    stationsByEvent.set(spec.id, {
      spec,
      handle,
      data: {
        id: spec.id,
        label: spec.label ?? spec.id,
        queuedCount: spec.queuedCount ?? 0,
        runCount: spec.runCount ?? 0,
        queued: [],
        runs: [],
      },
    })
  }

  // ─── Routing table ─────────────────────────────────────────────
  // Built AFTER all `.next` wiring above so every segment.next array
  // is populated. BFS from each station's east-output segment to
  // every other station's west-input segment yields the itinerary
  // (and per-pair travel duration) the snapshot reconciler uses for
  // chip placement.
  const routingTable: RoutingTable = buildRoutingTable(
    stationRows
      .filter(({ spec }) => spec.id != null)
      .map(({ spec, handle }) => ({
        id: spec.id!,
        exit: handle.ports[1].segment!,
        entry: handle.ports[0].segment!,
      })),
    BELT_WORLD_SPEED,
  )

  // ─── Snapshot reconciler ───────────────────────────────────────
  // Single source of truth for what's parked where and what's in
  // flight. Runs every frame off the latest snapshot:
  //   1. project each entity via placeEntity → parked|transit
  //   2. update tray counts / click-replay data only when changed
  //   3. reconcile chip meshes against the live transit set
  //
  // The same projection drives both station counts and chip
  // positions, so they cannot drift out of sync. WS events on the
  // page just trigger a snapshot refetch — they don't drive any
  // visual state directly.
  const knownStations = new Set<string>(stationsByEvent.keys())
  const chipController = renderer.getSnapshotChipController()
  let latestSnapshot: FactorySnapshot | null = null
  const lastTrayState = new Map<string, { queuedCount: number; runCount: number }>()

  // Live drawer feed. Per-station observer sets fire when the
  // station's content (queued entities + active runs) actually
  // changes, deduped by a content hash so idle frames don't churn
  // React re-renders. Empty observer sets short-circuit the hash
  // computation, so non-picked stations cost nothing per frame.
  const stationObservers = new Map<string, Set<(info: ClickedStationInfo) => void>>()
  const lastFiredHash = new Map<string, string>()
  const hashStationData = (data: ClickedStationInfo): string => {
    const queuedIds = data.queued.map((e) => e.id).join(',')
    const runIds = data.runs.map((r) => `${r.run.ID}:${r.run.Status}`).join(',')
    return `${data.queuedCount}|${data.runCount}|${queuedIds}|${runIds}`
  }

  const reconcile = (): void => {
    if (!latestSnapshot) return
    const now = Date.now()
    const transits = new Map<string, TransitPlacement>()
    const parkedByStation = new Map<string, FactoryEntity[]>()

    for (const e of latestSnapshot.entities) {
      const p = placeEntity(e, knownStations, routingTable.getDuration, now)
      if (!p) continue
      if (p.kind === 'parked') {
        const arr = parkedByStation.get(p.station)
        if (arr) arr.push(e)
        else parkedByStation.set(p.station, [e])
      } else {
        transits.set(e.id, {
          from: p.from,
          to: p.to,
          progress: p.progress,
          decor: chipDecorFor(e),
        })
      }
    }

    // Update station tray data. setQueuedCount/setRunCount touch a
    // DynamicTexture; only call them when the count actually changes
    // to avoid texture re-uploads on idle frames. Click-replay data
    // is always refreshed so a same-count-different-entities case
    // (one entity in, one out) reflects in the next drawer open.
    for (const [eventType, row] of stationsByEvent) {
      const fs = latestSnapshot.stations[eventType]
      const runs = fs?.runs ?? []
      const runEntityIds = new Set(runs.map((r) => r.task.entity_id))
      const allParked = parkedByStation.get(eventType) ?? []
      const queued = allParked.filter((e) => !runEntityIds.has(e.id))
      const queuedCount = queued.length
      const runCount = runs.length

      const last = lastTrayState.get(eventType)
      if (!last || last.queuedCount !== queuedCount || last.runCount !== runCount) {
        row.handle.setQueuedCount(queuedCount)
        row.handle.setRunCount(runCount)
        lastTrayState.set(eventType, { queuedCount, runCount })
      }
      // Lifetime counter: the station-handle API exposes this for every
      // station, so we update it unconditionally here with the latest
      // snapshot value when present.
      row.handle.setLifetimeCount(fs?.items_lifetime ?? 0)
      row.data = {
        id: eventType,
        label: row.spec.label ?? eventType,
        queuedCount,
        runCount,
        queued,
        runs,
      }

      // Fire live-drawer observers for this station only if anyone
      // is actually subscribed AND the content hash differs from
      // last fire. Most frames hit the empty-observers short-circuit.
      const observers = stationObservers.get(eventType)
      if (observers && observers.size > 0) {
        const key = hashStationData(row.data)
        if (lastFiredHash.get(eventType) !== key) {
          lastFiredHash.set(eventType, key)
          for (const cb of observers) cb(row.data)
        }
      }
    }

    chipController.reconcile(transits, (from, to) => routingTable.getItinerary(from, to))
  }

  const reconcileObserver = renderer.scene.onBeforeRenderObservable.add(reconcile)

  // ─── Cinematic mode ────────────────────────────────────────────
  // Ambient shot-director (factory/cinematic.ts). The deck is authored
  // against this floor's grid constants: the subject is the
  // architecture — the two hero bridges, the long belt runs, the loop,
  // the review cluster — and live chip motion only *weights* which
  // scenic shot comes next (it never picks the shot, never gets chased).
  const worldXY = (col: number, row: number, z = 0): Vector3 =>
    new Vector3(col * FLOOR_CELL, row * FLOOR_CELL, z)
  const stationCenter = (s: Station, z = 0): Vector3 => new Vector3(s.x + s.w / 2, s.y + s.d / 2, z)
  // Floor center shifted south to match the camera's home target
  // (INITIAL_TARGET_Y_OFFSET = -800 in iso-renderer) — where the
  // machinery actually sits, not the geometric floor center.
  const actionCenter = new Vector3(FLOOR_SIZE / 2, FLOOR_SIZE / 2 - 800, 0)

  // CI Passed station. This shot was originally the S1 bridge close-up,
  // but at a low angle it read as the splitter junction at the bridge's
  // foot, so it now frames CI Passed — just east of those splitters.
  // (The over/under crossing remains the dedicated bridge shot.)
  const ciPassedCenter = stationCenter(CI_PASSED, 30)
  // Over/under crossing — one belt passes under the span, one over.
  const crossingCenter = worldXY(
    (BRIDGE_UNDER_POLE_1_COL + OVER_BRIDGE_MERGER_COL) / 2 + 0.5,
    (BRIDGE_UNDER_POLE_1_ROW + OVER_BRIDGE_MERGER_ROW) / 2 + 0.5,
    14,
  )
  // Merger just east of CI Failed, where its output meets the loop-back
  // traffic — the subject of a tight, slowly rotating close-up.
  const ciFailedMergerCenter = worldXY(LOOP_MERGER_COL + 0.5, LOOP_MERGER_ROW + 0.5, 12)

  // A belt-truck shot: the camera holds a low angle while the target
  // glides end→end along a long run at a steady ~220 wu/s.
  const truckShot = (id: string, from: Vector3, to: Vector3): Shot => {
    const region = {
      cx: (from.x + to.x) / 2,
      cy: (from.y + to.y) / 2,
      halfX: Math.abs(to.x - from.x) / 2 + FLOOR_CELL,
      halfY: Math.abs(to.y - from.y) / 2 + FLOOR_CELL,
    }
    return {
      id,
      base: 7,
      region,
      resolve: () => {
        // Don't ride an empty belt. A truck shot is only worth it when
        // chips are actually traveling the run — otherwise it's a slow
        // pan over a dead conveyor. (Static structures like the bridges
        // read fine empty; a motion shot needs motion.) Declining here
        // makes the director re-pick something with life in it.
        const live = chipController
          .collectChipXY()
          .some(
            (c) =>
              Math.abs(c.x - region.cx) <= region.halfX &&
              Math.abs(c.y - region.cy) <= region.halfY,
          )
        if (!live) return null
        return {
          alpha: Math.PI / 2,
          beta: 0.6,
          radius: 720,
          target: from.clone(),
          targetTo: to.clone(),
          hold: Math.min(11, Math.max(6, Vector3.Distance(from, to) / 220)),
        }
      },
    }
  }

  const cinematicDeck: Shot[] = [
    {
      id: 'ci-passed',
      base: 7,
      region: {
        cx: ciPassedCenter.x,
        cy: ciPassedCenter.y,
        halfX: 2.5 * FLOOR_CELL,
        halfY: 2.5 * FLOOR_CELL,
      },
      resolve: () => ({
        alpha: Math.PI / 2 + 0.4,
        beta: 0.7,
        radius: 380,
        target: ciPassedCenter.clone(),
        hold: 6,
        driftAlpha: 0.04,
      }),
    },
    {
      id: 'crossing',
      base: 10,
      region: {
        cx: crossingCenter.x,
        cy: crossingCenter.y,
        halfX: 2.5 * FLOOR_CELL,
        halfY: 3.5 * FLOOR_CELL,
      },
      resolve: () => ({
        alpha: Math.PI / 2 - 0.5,
        beta: 0.72,
        radius: 480,
        target: crossingCenter.clone(),
        hold: 7,
        driftAlpha: 0.04,
      }),
    },
    {
      // Ultra close-up on the merger just east of CI Failed, orbiting
      // slowly L→R. Tightest zoom the camera allows; reads as a detailed
      // junction even when idle, so it helps fill the quiet-floor rotation.
      id: 'ci-failed-merger',
      base: 8,
      region: {
        cx: ciFailedMergerCenter.x,
        cy: ciFailedMergerCenter.y,
        halfX: 2 * FLOOR_CELL,
        halfY: 2 * FLOOR_CELL,
      },
      resolve: () => ({
        alpha: Math.PI / 2,
        beta: 1.1,
        radius: 270, // ≈ the camera's tightest zoom (lowerRadiusLimit)
        target: ciFailedMergerCenter.clone(),
        hold: 9,
        driftAlpha: -0.05, // slow sweep (reversed)
      }),
    },
    truckShot(
      'truck-rfr',
      worldXY(RFR_SPLITTER_COL + 0.5, RFR_SPLITTER_ROW + 0.5),
      worldXY(RFR_POLE2_COL + 0.5, RFR_POLE2_ROW + 0.5),
    ),
    truckShot(
      'truck-loop',
      worldXY(LOOP_POLE2_COL + 0.5, LOOP_POLE2_ROW + 0.5),
      worldXY(LOOP_POLE3_COL + 0.5, LOOP_POLE3_ROW + 0.5),
    ),
    truckShot(
      'truck-mc',
      worldXY(MC_FOOT_MERGER_COL + 0.5, MC_FOOT_MERGER_ROW + 0.5),
      worldXY(CR_MERGER_COL + 0.5, CR_MERGER_ROW + 0.5),
    ),
    {
      id: 'review-crane',
      base: 6,
      region: {
        cx: stationCenter(REVIEW_REQUESTED).x,
        cy: stationCenter(REVIEW_REQUESTED).y,
        halfX: 3 * FLOOR_CELL,
        halfY: 3 * FLOOR_CELL,
      },
      resolve: () => ({
        alpha: Math.PI / 2 + 0.3,
        beta: 0.45,
        radius: 640,
        target: stationCenter(REVIEW_REQUESTED, 0),
        hold: 6.5,
        craneBeta: 0.06, // 0.45 → ~0.84 over the hold
        driftAlpha: 0.03,
      }),
    },
    {
      id: 'establishing',
      base: 5,
      region: null,
      wide: true,
      resolve: () => ({
        alpha: Math.PI / 2,
        beta: 0.52,
        radius: 1600,
        target: actionCenter.clone(),
        hold: 8,
        driftAlpha: 0.02,
      }),
    },
    {
      id: 'blueprint',
      base: 5,
      region: null,
      wide: true,
      resolve: () => ({
        alpha: Math.PI / 2,
        beta: 0.18,
        radius: 1900,
        target: actionCenter.clone(),
        hold: 9,
        driftTargetX: 16,
      }),
    },
    {
      id: 'station-detail',
      base: 3,
      region: null,
      resolve: () => {
        // Frame the station doing the most agent work — gated on active
        // runs, never queued piles (a heap is not a spectacle). runCount
        // is refreshed every frame by the reconciler (registered on
        // onBeforeRender before the director's tick, so it's current here);
        // a frame of staleness would be harmless either way since runs
        // turn over on the order of seconds.
        let best: StationRow | null = null
        for (const row of stationsByEvent.values()) {
          if ((row.data.runCount ?? 0) <= 0) continue
          if (!best || row.data.runCount > best.data.runCount) best = row
        }
        if (!best) return null
        return {
          alpha: Math.PI / 2 + 0.4,
          beta: 0.72,
          radius: 360,
          target: stationCenter(best.spec, 20),
          hold: 5.5,
          driftAlpha: 0.05,
        }
      },
    },
  ]

  const cinematic = new CinematicDirector(
    renderer.camera,
    renderer.scene,
    cinematicDeck,
    () => chipController.collectChipXY(),
    (opacity) => {
      fadeOverlay.style.opacity = String(opacity)
    },
  )
  // Re-attach user camera controls once the camera has fully eased back
  // to the pre-entry pose (fires on every exit, manual or self).
  cinematic.onEnd(() => renderer.camera.attachControl(true))

  const ro = new ResizeObserver(() => {
    renderer.resize()
  })
  ro.observe(container)

  return {
    destroy: () => {
      renderer.scene.onBeforeRenderObservable.remove(reconcileObserver)
      cinematic.dispose()
      ro.disconnect()
      renderer.destroy()
      fadeOverlay.remove()
      canvas.remove()
    },
    resetView: () => renderer.resetView(),
    onCameraChange: (cb) => {
      // Throttle to one notification per animation frame — Babylon's
      // observable can fire on every input pixel.
      let raf: number | null = null
      const snapshot = (): CameraStateForHUD => ({
        pitch: renderer.camera.beta,
        yaw: renderer.camera.alpha,
        zoom: INITIAL_ZOOM_RADIUS / renderer.camera.radius,
      })
      const wrapped = () => {
        if (raf != null) return
        raf = requestAnimationFrame(() => {
          raf = null
          cb(snapshot())
        })
      }
      const observer = renderer.camera.onViewMatrixChangedObservable.add(wrapped)
      cb(snapshot())
      return () => {
        if (raf != null) cancelAnimationFrame(raf)
        renderer.camera.onViewMatrixChangedObservable.remove(observer)
      }
    },
    onStationClick: (cb) => {
      return renderer.onStationClick((stationId) => {
        if (stationId == null) {
          cb(null)
          return
        }
        const row = stationsByEvent.get(stationId)
        cb(row?.data ?? null)
      })
    },
    onStationDataChange: (stationId, cb) => {
      let set = stationObservers.get(stationId)
      if (!set) {
        set = new Set()
        stationObservers.set(stationId, set)
      }
      set.add(cb)
      return () => {
        const s = stationObservers.get(stationId)
        if (!s) return
        s.delete(cb)
        if (s.size === 0) {
          // Drop the dedup hash too so a future re-subscription gets
          // the next change cleanly rather than comparing against a
          // hash from an old session.
          stationObservers.delete(stationId)
          lastFiredHash.delete(stationId)
        }
      }
    },
    applySnapshot: (snapshot) => {
      latestSnapshot = snapshot
      // Run a reconciliation right away so the first paint after a
      // refetch reflects the new data without waiting on the next
      // frame (matters mostly for the initial load, where the user
      // hasn't seen anything yet).
      reconcile()
    },
    enterCinematic: (reduced = false) => {
      // Detach user input so a stray pointer/wheel can't fight the
      // director. The React exit-catcher overlay is the primary guard;
      // this is the backstop. Re-attached via cinematic.onEnd above.
      renderer.camera.detachControl()
      cinematic.enter(reduced)
    },
    exitCinematic: () => cinematic.exit(),
    onCinematicEnd: (cb) => cinematic.onEnd(cb),
  }
}

// Compute chip decoration (hue + short label) from a snapshot entity.
// GitHub: hue from the repo, label is the PR number. Jira: hue from
// the project key prefix of source_id, label is the full source_id.
// Same hue for every chip belonging to the same repo/project keeps
// them visually grouped without any user config.
function chipDecorFor(entity: FactoryEntity): { hue?: number; label?: string } {
  if (entity.source === 'github') {
    return {
      hue: entity.repo ? hashHue(entity.repo) : undefined,
      label: entity.number != null ? `#${entity.number}` : undefined,
    }
  }
  if (entity.source === 'jira') {
    const projectKey = entity.source_id?.split('-')[0]
    return {
      hue: projectKey ? hashHue(projectKey) : undefined,
      label: entity.source_id || undefined,
    }
  }
  return {}
}

// hashHue is re-exported from iso-items so callers (the React-layer
// spawn pipeline) can derive a chip hue from a repo / project string
// without reaching into the simulator module directly.
export { hashHue }
