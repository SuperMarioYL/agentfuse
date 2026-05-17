# assets/

Demo assets that go into the README at launch. Placeholders for now — the
record-and-export step happens once the binary is buildable on the launch
machine.

| File | Purpose | How to record |
| --- | --- | --- |
| `demo.gif` | 30s top-of-README GIF: `fuse init` → `fuse run claude` → cap hits → 402 | record with [`vhs`](https://github.com/charmbracelet/vhs) using `demo.tape` |
| `demo.tape` | `vhs` script that reproduces the GIF deterministically | hand-write, commit |
| `arch.svg`  | The "how it works" block diagram now drawn in README ASCII | export from excalidraw/draw.io |
| `logo.svg`  | Wordmark for repo social card | hand-draw / hire later |

## `demo.tape` skeleton (to fill in)

```
Output assets/demo.gif
Set FontSize 22
Set Width 1100
Set Height 640

Type "fuse init --cap 5"
Enter
Sleep 1s

Type "fuse run claude"
Enter
Sleep 2s
# ... agent does a few turns; final turn trips the cap; HTTP 402 on stderr ...
```

The point of the GIF is one frame: the line
`agentfuse: budget exceeded for project … ($5.02 / $5.00) — raise with: fuse cap +5`.
Everything else is setup.
