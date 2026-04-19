---
type: "query"
date: "2026-04-19T16:01:13.102091+00:00"
question: "Why does Tracker bridge 7 communities - god object or legitimate coordinator?"
contributor: "graphify"
source_nodes: ["tracker_tracker", "groupsession_groupsession"]
---

# Q: Why does Tracker bridge 7 communities - god object or legitimate coordinator?

## Answer

Tracker has 89 edges but 87 are 'method' edges from Go receiver syntax - a structural artifact, not 89 responsibilities. All edges EXTRACTED, none INFERRED or AMBIGUOUS. CLAUDE.md confirms Tracker is intentionally the central struct, guarding session/users/aprs/devices under one mutex. However the graph reveals real multi-concern coupling: APRS goroutines, ~30 Telegram handlers, DM flow, state I/O, and dispatch guards all hang off the same struct. Verdict: legitimate coordinator, not a random god object, but a clean refactor would extract a Handlers struct (holding *Tracker) for all cmd*/cb*/exec* methods. Communities 4/5/6/7 (Confirmations, Session Lifecycle, Guards, Callback Dispatch) are almost entirely handler-layer and would move cleanly.

## Source Nodes

- tracker_tracker
- groupsession_groupsession