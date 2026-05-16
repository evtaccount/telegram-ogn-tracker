# Graph Report - /Users/EVT/Developer/pet_projects/telegram-ogn-tracker  (2026-05-16)

## Corpus Check
- 14 files · ~56,589 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 211 nodes · 615 edges · 23 communities detected
- Extraction: 47% EXTRACTED · 53% INFERRED · 0% AMBIGUOUS · INFERRED: 329 edges (avg confidence: 0.8)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- [[_COMMUNITY_Community 0|Community 0]]
- [[_COMMUNITY_Community 1|Community 1]]
- [[_COMMUNITY_Community 2|Community 2]]
- [[_COMMUNITY_Community 3|Community 3]]
- [[_COMMUNITY_Community 4|Community 4]]
- [[_COMMUNITY_Community 5|Community 5]]
- [[_COMMUNITY_Community 6|Community 6]]
- [[_COMMUNITY_Community 7|Community 7]]
- [[_COMMUNITY_Community 8|Community 8]]
- [[_COMMUNITY_Community 9|Community 9]]
- [[_COMMUNITY_Community 10|Community 10]]
- [[_COMMUNITY_Community 11|Community 11]]
- [[_COMMUNITY_Community 12|Community 12]]
- [[_COMMUNITY_Community 13|Community 13]]
- [[_COMMUNITY_Community 14|Community 14]]
- [[_COMMUNITY_Community 15|Community 15]]
- [[_COMMUNITY_Community 16|Community 16]]
- [[_COMMUNITY_Community 17|Community 17]]
- [[_COMMUNITY_Community 18|Community 18]]
- [[_COMMUNITY_Community 19|Community 19]]
- [[_COMMUNITY_Community 20|Community 20]]
- [[_COMMUNITY_Community 21|Community 21]]
- [[_COMMUNITY_Community 22|Community 22]]

## God Nodes (most connected - your core abstractions)
1. `Tracker` - 108 edges
2. `commandArgs()` - 9 edges
3. `shortID()` - 8 edges
4. `formatTrackText()` - 7 edges
5. `NewTracker()` - 7 edges
6. `main()` - 6 edges
7. `formatDDBInfo()` - 6 edges
8. `isPrivateChat()` - 6 edges
9. `buildSummary()` - 6 edges
10. `pilotButtons()` - 6 edges

## Surprising Connections (you probably didn't know these)
- `shortID()` --calls--> `TestShortID()`  [INFERRED]
  /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/util.go → /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/tracker_test.go
- `isValidShortID()` --calls--> `TestIsValidShortID()`  [INFERRED]
  /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/util.go → /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/tracker_test.go
- `isMessageNotModified()` --calls--> `TestIsMessageNotModified()`  [INFERRED]
  /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/util.go → /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/tracker_test.go
- `isMessageGone()` --calls--> `TestIsMessageGone()`  [INFERRED]
  /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/util.go → /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/tracker_test.go
- `formatDDBInfo()` --calls--> `TestFormatDDBInfo()`  [INFERRED]
  /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/util.go → /Users/EVT/Developer/pet_projects/telegram-ogn-tracker/internal/tracker/tracker_test.go

## Hyperedges (group relationships)
- **Tracking goroutine lifecycle trio (filter + APRS + ticker)** — tracker_updatefilter, client_runclient, client_sendupdates [EXTRACTED 0.95]
- **DM self-add flow across group + DM handlers** — commands_cmdadd, commands_cmdstartprivate, tracker_handledmtext, commands_cmdconfirm [EXTRACTED 0.90]
- **Radar mode pipeline (client + updater + render)** — client_runradarclient, client_sendradarupdates, client_buildradarsummary, client_radarbuttons [EXTRACTED 0.90]

## Communities

### Community 0 - "Community 0"
Cohesion: 0.05
Nodes (31): buildFilter(), nextReconnectDelay(), shouldAttemptPin(), updateLandingState(), writeStateBytes(), appState, landingEvent, legacySessionState (+23 more)

### Community 1 - "Community 1"
Cohesion: 0.09
Nodes (27): buildDashboard(), buildRadarSummary(), buildSummary(), dashboardButtons(), formatTrackText(), nearestDriver(), pilotButtons(), radarButtons() (+19 more)

### Community 2 - "Community 2"
Cohesion: 0.19
Nodes (4): commandArgs(), isPrivateChat(), isValidShortID(), shortID()

### Community 3 - "Community 3"
Cohesion: 0.22
Nodes (0): 

### Community 4 - "Community 4"
Cohesion: 0.27
Nodes (1): Tracker

### Community 5 - "Community 5"
Cohesion: 0.24
Nodes (1): deleteCallbackMessage()

### Community 6 - "Community 6"
Cohesion: 0.27
Nodes (1): TestSessionResetClearsDashboard()

### Community 7 - "Community 7"
Cohesion: 0.36
Nodes (1): TestPendingCleanupQueue()

### Community 8 - "Community 8"
Cohesion: 0.29
Nodes (4): stdlogClassifier, main(), openLogSink(), TestIsAllowedChat()

### Community 9 - "Community 9"
Cohesion: 0.25
Nodes (6): Coordinates, DriverInfo, GroupSession, PilotStatus, RadarEntry, UserInfo

### Community 10 - "Community 10"
Cohesion: 1.0
Nodes (0): 

### Community 11 - "Community 11"
Cohesion: 1.0
Nodes (0): 

### Community 12 - "Community 12"
Cohesion: 1.0
Nodes (0): 

### Community 13 - "Community 13"
Cohesion: 1.0
Nodes (0): 

### Community 14 - "Community 14"
Cohesion: 1.0
Nodes (1): Landing auto-detection heuristic

### Community 15 - "Community 15"
Cohesion: 1.0
Nodes (1): APRS budlist+range filter logic

### Community 16 - "Community 16"
Cohesion: 1.0
Nodes (1): DM Add Flow (deep-link pilot self-add)

### Community 17 - "Community 17"
Cohesion: 1.0
Nodes (1): Radar mode (parallel APRS stream)

### Community 18 - "Community 18"
Cohesion: 1.0
Nodes (1): Tracking goroutine lifecycle

### Community 19 - "Community 19"
Cohesion: 1.0
Nodes (1): State persistence & auto-resume

### Community 20 - "Community 20"
Cohesion: 1.0
Nodes (1): 6-char OGN ID matching convention

### Community 21 - "Community 21"
Cohesion: 1.0
Nodes (1): /start must not wipe existing session

### Community 22 - "Community 22"
Cohesion: 1.0
Nodes (1): Re-create APRS client after Disconnect (killed flag)

## Knowledge Gaps
- **21 isolated node(s):** `landingEvent`, `PilotStatus`, `RadarEntry`, `Coordinates`, `DriverInfo` (+16 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **Thin community `Community 10`** (1 nodes): `cleanup.go`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 11`** (1 nodes): `dm.go`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 12`** (1 nodes): `flows.go`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 13`** (1 nodes): `commands.go`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 14`** (1 nodes): `Landing auto-detection heuristic`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 15`** (1 nodes): `APRS budlist+range filter logic`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 16`** (1 nodes): `DM Add Flow (deep-link pilot self-add)`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 17`** (1 nodes): `Radar mode (parallel APRS stream)`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 18`** (1 nodes): `Tracking goroutine lifecycle`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 19`** (1 nodes): `State persistence & auto-resume`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 20`** (1 nodes): `6-char OGN ID matching convention`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 21`** (1 nodes): `/start must not wipe existing session`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 22`** (1 nodes): `Re-create APRS client after Disconnect (killed flag)`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Tracker` connect `Community 4` to `Community 0`, `Community 1`, `Community 2`, `Community 3`, `Community 5`, `Community 6`, `Community 7`, `Community 8`?**
  _High betweenness centrality (0.486) - this node is a cross-community bridge._
- **Why does `TrackInfo` connect `Community 1` to `Community 9`, `Community 2`?**
  _High betweenness centrality (0.069) - this node is a cross-community bridge._
- **Are the 8 inferred relationships involving `commandArgs()` (e.g. with `.cmdStartPrivate()` and `.cmdMyID()`) actually correct?**
  _`commandArgs()` has 8 INFERRED edges - model-reasoned connections that need verification._
- **Are the 7 inferred relationships involving `shortID()` (e.g. with `.cmdMyID()` and `.runClient()`) actually correct?**
  _`shortID()` has 7 INFERRED edges - model-reasoned connections that need verification._
- **Are the 4 inferred relationships involving `formatTrackText()` (e.g. with `.StatusEmoji()` and `formatDDBInfo()`) actually correct?**
  _`formatTrackText()` has 4 INFERRED edges - model-reasoned connections that need verification._
- **Are the 4 inferred relationships involving `NewTracker()` (e.g. with `main()` and `.saveWorker()`) actually correct?**
  _`NewTracker()` has 4 INFERRED edges - model-reasoned connections that need verification._
- **What connects `landingEvent`, `PilotStatus`, `RadarEntry` to the rest of the system?**
  _21 weakly-connected nodes found - possible documentation gaps or missing edges._