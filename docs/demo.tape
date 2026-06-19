# vhs tape — AgentFuse happy path.
# Render in CI via .github/workflows/demo.yml,
# or locally: `vhs docs/demo.tape` (needs the `fuse` binary on PATH).
Output assets/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1100
Set Height 640
Set Theme "Dracula"
Set Padding 18

Type "# 1) Drop a $5 cap into the project — no daemon, no telemetry"
Enter
Sleep 600ms
Type "fuse init --cap 5"
Enter
Sleep 2s

Type "# 2) Inspect the cap, today's spend and remaining budget"
Enter
Sleep 600ms
Type "fuse status"
Enter
Sleep 2.5s

Type "# 3) Raise the cap atomically when you hit the wall"
Enter
Sleep 600ms
Type "fuse cap +5"
Enter
Sleep 2s

Type "# 4) Confirm the new ceiling"
Enter
Sleep 600ms
Type "fuse status"
Enter
Sleep 2.5s

Type "# Then: fuse run claude  — proxies your agent CLI and fails closed at the cap"
Enter
Sleep 2s
