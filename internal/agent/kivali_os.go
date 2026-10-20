package agent

import _ "embed"

// KivaliOS is the document every agent's system prompt opens with: how
// the runtime works — what wakes an agent, how messages and
// assignments move, where files and memory live, what each tool does
// and what the server checks. It ships in the binary, so every install
// reads the current text the moment a release runs; what is the team's
// to decide is in its handbook (internal/seed holds the defaults, the
// store holds the text). Agents are told it says what is and the
// handbook and their role say what to do.
// Edit internal/agent/kivali_os.md to change it.
//
//go:embed kivali_os.md
var KivaliOS string
