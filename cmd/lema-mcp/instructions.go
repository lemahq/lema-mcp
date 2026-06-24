package main

// authedServerInstructions is the server-level steering for the AUTHED `lema`
// server — the superset that also serves the public tools. The public funnel's
// equivalent is publicServerInstructions (try.go); the authed server shipped with
// NONE (main.go passed nil ServerOptions), so an agent on a connected repo got no
// proactive priming at all — the largest single lever on the measured dead-tool
// problem (ADR-0124, the v1 read wedge). It states the failure mode as FACT, not as
// an instruction to Claude, so it stays clear of the Directory banned-phrase rule;
// it names no specific tool, so it survives the tool renames.
const authedServerInstructions = "lema answers from your team's own recorded decisions — what was chosen, what was ruled out, and why — grounded in the record with citations. A coding agent cannot recover a team's rationale or its rejected alternatives from the source code; producing a \"why\" from model recall reconstructs it — fluently, sometimes wrongly. This server returns the recorded reasoning instead. When you reach a design choice — a library, a pattern, an architecture, a direction — the team may have already settled or ruled on it, and this server holds that record. It holds reasoning (why a decision was made, what was rejected), not API syntax or code samples — a documentation tool is the right place for those. When the record is silent it says so; that means \"unknown,\" not \"approved.\" It also answers the same way for the public upstream projects React, Kubernetes, and Rust."
