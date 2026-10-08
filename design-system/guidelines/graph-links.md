Rules for drawing the knowledge graph's links, for whichever view shows them (a dependency lens, an overview, an inline preview).

- **Rests on** is a solid 1.5px `ink-faint` line with a small arrowhead pointing at what is depended on.
- **Supersedes** is the same line dashed (5/4), pointing from the old node to the one that replaced it; the old node is shown superseded (struck through, faded).
- **The selected path** (from a selected node to everything it rests on, or everything resting on it) is 2.5px `cobalt`, and its end nodes get the `cobalt` outline. Everything else stays quiet.
- **Unresolved** links, to something that doesn't exist, are dotted `danger` lines ending in a label naming what's missing; the node gets the dashed `unresolved` edge.
- **Owners** are soft clusters in the owning agent's identity color at 10 to 12% strength with the agent's name in mono, never a hard border.
- Links never carry meaning by color alone: line style (solid, dashed, dotted) is the primary cue, color the second.
