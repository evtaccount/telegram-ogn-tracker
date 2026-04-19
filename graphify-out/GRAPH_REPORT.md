# Graph Report - .  (2026-04-19)

## Corpus Check
- Corpus is ~14,771 words - fits in a single context window. You may not need a graph.

## Summary
- 173 nodes · 465 edges · 15 communities detected
- Extraction: 68% EXTRACTED · 32% INFERRED · 0% AMBIGUOUS · INFERRED: 148 edges (avg confidence: 0.8)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- [[_COMMUNITY_Tracking Loop & Landing Detection|Tracking Loop & Landing Detection]]
- [[_COMMUNITY_DM & Shared Helpers|DM & Shared Helpers]]
- [[_COMMUNITY_Bot Entry & Add Flow|Bot Entry & Add Flow]]
- [[_COMMUNITY_Radar & List UI|Radar & List UI]]
- [[_COMMUNITY_Action Confirmations|Action Confirmations]]
- [[_COMMUNITY_Session Lifecycle|Session Lifecycle]]
- [[_COMMUNITY_Group Command Guards|Group Command Guards]]
- [[_COMMUNITY_Callback Dispatch|Callback Dispatch]]
- [[_COMMUNITY_State Persistence|State Persistence]]
- [[_COMMUNITY_GroupSession Keyboard|GroupSession Keyboard]]
- [[_COMMUNITY_DM Reply Keyboard|DM Reply Keyboard]]
- [[_COMMUNITY_Track On Exec|Track On Exec]]
- [[_COMMUNITY_Track Off Exec|Track Off Exec]]
- [[_COMMUNITY_Session Guard|Session Guard]]
- [[_COMMUNITY_Group Chat Guard|Group Chat Guard]]

## God Nodes (most connected - your core abstractions)
1. `Tracker` - 89 edges
2. `GroupSession` - 14 edges
3. `shortID()` - 11 edges
4. `Tracker.runClient (OGN APRS loop)` - 10 edges
5. `Tracker.updateFilter` - 9 edges
6. `formatDDBInfo()` - 8 edges
7. `NewTracker()` - 8 edges
8. `commandArgs()` - 8 edges
9. `TrackInfo` - 7 edges
10. `Tracker.RegisterHandlers` - 7 edges

## Surprising Connections (you probably didn't know these)
- `shortID()` --implements--> `6-char OGN ID matching convention`  [EXTRACTED]
  internal/tracker/tracker.go → README.md
- `Tracker.updateFilter` --implements--> `APRS budlist+range filter logic`  [EXTRACTED]
  internal/tracker/tracker.go → CLAUDE.md
- `Re-create APRS client after Disconnect (killed flag)` --rationale_for--> `Tracker.updateFilter`  [EXTRACTED]
  DECISIONS.md → internal/tracker/tracker.go
- `Tracker.runClient (OGN APRS loop)` --implements--> `Landing auto-detection heuristic`  [EXTRACTED]
  internal/tracker/client.go → CLAUDE.md
- `Tracker.saveState (JSON atomic write)` --implements--> `State persistence & auto-resume`  [EXTRACTED]
  internal/tracker/persist.go → CLAUDE.md

## Hyperedges (group relationships)
- **Tracking goroutine lifecycle trio (filter + APRS + ticker)** — tracker_updatefilter, client_runclient, client_sendupdates [EXTRACTED 0.95]
- **DM self-add flow across group + DM handlers** — commands_cmdadd, commands_cmdstartprivate, tracker_handledmtext, commands_cmdconfirm [EXTRACTED 0.90]
- **Radar mode pipeline (client + updater + render)** — client_runradarclient, client_sendradarupdates, client_buildradarsummary, client_radarbuttons [EXTRACTED 0.90]

## Communities

### Community 0 - "Tracking Loop & Landing Detection"
Cohesion: 0.09
Nodes (29): Tracker.buildRadarSummary, Tracker.buildSummary, Tracker.formatTrackText, landingEvent value type, nearestDriver(), Tracker.runClient (OGN APRS loop), Tracker.runRadarClient, Tracker.sendLandingAlert (+21 more)

### Community 1 - "DM & Shared Helpers"
Cohesion: 0.18
Nodes (4): 6-char OGN ID matching convention, commandArgs(), isPrivateChat(), shortID()

### Community 2 - "Bot Entry & Add Flow"
Cohesion: 0.1
Nodes (24): cmdAdd (/add dispatcher), cmdConfirm (DM confirm OGN ID), cmdDebugWipe, cmdMyID, cmdRemove, cmdStart (/start router), cmdStartPrivate (DM deep-link), execAddDirect (inline /add id) (+16 more)

### Community 3 - "Radar & List UI"
Cohesion: 0.15
Nodes (5): pilotButtons(), radarButtons(), landingEvent, mapsNavURL(), radarLine

### Community 4 - "Action Confirmations"
Cohesion: 0.26
Nodes (1): Tracker

### Community 5 - "Session Lifecycle"
Cohesion: 0.19
Nodes (0): 

### Community 6 - "Group Command Guards"
Cohesion: 0.22
Nodes (1): isGroupChat()

### Community 7 - "Callback Dispatch"
Cohesion: 0.25
Nodes (1): deleteCallbackMessage()

### Community 8 - "State Persistence"
Cohesion: 0.33
Nodes (5): appState, legacySessionState, pilotState, sessionState, userState

### Community 9 - "GroupSession Keyboard"
Cohesion: 1.0
Nodes (1): GroupSession.replyKeyboard

### Community 10 - "DM Reply Keyboard"
Cohesion: 1.0
Nodes (1): Tracker.dmReplyKeyboard

### Community 11 - "Track On Exec"
Cohesion: 1.0
Nodes (1): cmdTrackOn

### Community 12 - "Track Off Exec"
Cohesion: 1.0
Nodes (1): cmdTrackOff

### Community 13 - "Session Guard"
Cohesion: 1.0
Nodes (1): requireSession guard

### Community 14 - "Group Chat Guard"
Cohesion: 1.0
Nodes (1): requireGroupChat guard

## Knowledge Gaps
- **25 isolated node(s):** `landingEvent`, `radarLine`, `appState`, `userState`, `sessionState` (+20 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **Thin community `GroupSession Keyboard`** (1 nodes): `GroupSession.replyKeyboard`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `DM Reply Keyboard`** (1 nodes): `Tracker.dmReplyKeyboard`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Track On Exec`** (1 nodes): `cmdTrackOn`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Track Off Exec`** (1 nodes): `cmdTrackOff`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Session Guard`** (1 nodes): `requireSession guard`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Group Chat Guard`** (1 nodes): `requireGroupChat guard`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Tracker` connect `Action Confirmations` to `Tracking Loop & Landing Detection`, `DM & Shared Helpers`, `Bot Entry & Add Flow`, `Radar & List UI`, `Session Lifecycle`, `Group Command Guards`, `Callback Dispatch`?**
  _High betweenness centrality (0.508) - this node is a cross-community bridge._
- **Why does `GroupSession` connect `Tracking Loop & Landing Detection` to `DM & Shared Helpers`, `Bot Entry & Add Flow`, `Action Confirmations`, `Session Lifecycle`?**
  _High betweenness centrality (0.159) - this node is a cross-community bridge._
- **Why does `Tracker.runClient (OGN APRS loop)` connect `Tracking Loop & Landing Detection` to `DM & Shared Helpers`, `Bot Entry & Add Flow`?**
  _High betweenness centrality (0.090) - this node is a cross-community bridge._
- **Are the 5 inferred relationships involving `shortID()` (e.g. with `.runClient()` and `.runRadarClient()`) actually correct?**
  _`shortID()` has 5 INFERRED edges - model-reasoned connections that need verification._
- **What connects `landingEvent`, `radarLine`, `appState` to the rest of the system?**
  _25 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Tracking Loop & Landing Detection` be split into smaller, more focused modules?**
  _Cohesion score 0.09 - nodes in this community are weakly interconnected._
- **Should `Bot Entry & Add Flow` be split into smaller, more focused modules?**
  _Cohesion score 0.1 - nodes in this community are weakly interconnected._