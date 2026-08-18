---
schema_version: lets.issue-tracker/v2
provider: github
host: github.com
repository: fml09/typeclaw-operator
repository_node_id: R_kgDOT8Ld7Q
trusted_actors:
  - fml09
managed_marker: "lets:managed/v1"
managed_block_start: "<!-- lets:managed/v1:start -->"
managed_block_end: "<!-- lets:managed/v1:end -->"
body_profile_marker: "lets:body-profile/v1"
spec:
  labels:
    - lets:spec
  issue_type_ids: []
ticket:
  labels:
    - lets:ticket
  issue_type_ids: []
wayfinding_map:
  labels:
    - lets:wayfind-map
  issue_type_ids: []
profiles:
  - id: lets.spec/v1
    kind: spec
    required_headings:
      - Problem Statement
      - Solution
      - User Stories
      - Implementation Decisions
      - Testing Decisions
    optional_headings:
      - Out of Scope
      - Further Notes
    allow_empty_headings:
      - Out of Scope
      - Further Notes
    decision_types: []
  - id: lets.delivery-ticket/v1
    kind: ticket
    required_headings:
      - Outcome
      - Scope
      - Acceptance Criteria
    optional_headings: []
    allow_empty_headings: []
    decision_types: []
  - id: lets.decision-ticket/v1
    kind: ticket
    required_headings:
      - Question
    optional_headings:
      - Context
    allow_empty_headings:
      - Context
    decision_types:
      - research
      - prototype
      - grilling
      - task
  - id: lets.wayfind-map/v1
    kind: wayfinding-map
    required_headings:
      - Destination
      - Notes
      - Decisions so far
      - Not yet specified
      - Out of scope
    optional_headings: []
    allow_empty_headings:
      - Notes
      - Decisions so far
      - Not yet specified
      - Out of scope
    decision_types: []
legacy_issues: []
legacy_evidence_profiles: []
workflow:
  ready_label: ready-for-agent
  relation_marker: "lets:relation/v1"
---

# GitHub tracker contract

GitHub.com is this repository's only managed Spec, Ticket, and Wayfinding Map
tracker. Local Markdown copies are not fallback authority.

A managed Issue must belong to the configured repository node, match exactly
one configured classification, contain one well-formed managed block and one
allowlisted body-profile marker, and have a trusted creator. Human prose
outside the managed block remains human-owned.

`legacy_issues` is empty because the live repository contained no Issues when
this contract was established.

Before relying on a managed Issue, reread its live GitHub original. Writes
follow the mutation contract carried by the workflow skill performing the
change: validate one bounded mutation, apply it once, then verify its
postcondition.
