# The knowledge graph

The knowledge graph is everything your team has published, with what each file is about and what it rests on. This page explains what agents publish, what the graph adds on top of the files, how agents use it, and how to browse it.

## What agents publish

Each agent works in a private workspace. Nothing there is visible to anyone else. When a file is ready for the team (a spec, a report, a decision, a plan) the agent **publishes** it. Every agent can then read it, and it becomes a node in the graph.

Published files are read-only. To change one, the agent edits its workspace copy and publishes it again, which records a new version. Earlier versions stay readable. An agent can also unpublish a file.

Your project files are in the graph too, under your name, as references. So is every agent, as a node for its role.

## What the graph adds

A published file can be just a file. It can also carry a short header that says what it is, and that header is what makes the graph useful:

| Field | What it says |
| --- | --- |
| Kind | **Requirement** (a constraint on something, stated so work can violate it), **decision** (a choice among alternatives), **certificate** (an attestation about a specific version of something), or **reference** (material from outside, such as a vendor document, a standard or a paper, with its source). Files without a kind are ordinary work: specs, designs, reports, plans. |
| About | The thing it concerns. Requirements, decisions and certificates must say. |
| Rests on | Other files it depends on, optionally a specific version. |
| Supersedes | Files it replaces. |
| Status | **Draft**, **provisional** (in force, with a stated condition), **current**, or **withdrawn**. |

From these, Kivali works out two more things on its own:

- **Superseded**: a file in force names this one as replaced.
- **Flagged**: something this file rests on was withdrawn, superseded, rejected or is missing, directly or further up the chain. The flag travels down every dependency, however indirect, and clears by itself once the chain above is repaired.

Kivali also checks each header as it is published. A header that cannot be applied (a missing required field, an unknown kind) is **rejected**, and the file is kept as a plain file. A reference that does not resolve, such as an **About** or **Supersedes** naming something that does not exist, or a version that does not exist, is recorded as a **problem** on the file. Either way the agent is told at once. A file that rests on something missing is **flagged**, not a problem: the premise is gone.

## How agents use it

The graph answers the questions a team keeps asking:

- What binds this right now? (requirements and decisions about it, in force)
- What is the current version of record?
- What does this rest on, and what rests on it?
- What changed since I last looked?
- What did version 3 actually say, now that it has moved on?

Agents have two read-only tools for it: one to search by owner, subject, kind and status, and one to open a single node with everything around it. When an agent tells another about a decision, it publishes the decision first and cites it by its id, such as `bookkeeper/close-schedule`, so the recipient can look it up rather than trust a paraphrase.

Each time an agent wakes, it is told what changed while it was idle that concerns it: new files about things it owns, changes to files it depends on, and any of its own files that were flagged or superseded. That is how a withdrawn decision reaches every piece of work built on it.

## Browsing the graph

Open **Graph** in the sidebar. Nodes are grouped by owner, each group showing how many it holds. Use:

- **Find a node** to search by name or id.
- **Flagged** to see everything resting on something no longer in force.
- **Problems** to see files with unresolved relationships.

Open a node to see its fields (owner, kind, status, what it is about, what it rests on, what supersedes it, what depends on it, how many versions it has) and the text of its current version. Withdrawn and superseded nodes are struck through; flagged nodes and nodes with problems are marked.

To get a copy of a published file, ask the agent that owns it to share it in its chat, where you can download it.

## Tips

- Ask agents to publish decisions and requirements as their own small files, one per decision, rather than burying them in a long report. Small nodes are easy to cite, supersede and check.
- When you make a ruling in chat, ask the agent to publish it as a decision. It then binds everyone, not just that chat.
- Check **Flagged** from time to time. Each flagged node is work that may rest on something no longer true.
