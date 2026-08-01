# DEBUG — provider visibility controls

**Status:** ✅ fixed

## Parts

### Part 1 — Fix TUI space toggle   ✅

Bubble Tea represents the space key as `KeyMsg.String() == " "`, not
`"space"`. Update the picker key match and add a regression test that sends a
space key message and verifies the selected provider is toggled.

### Part 2 — Group tray provider controls   ✅

Put provider checkbox items below a `Toggle providers` systray submenu using
`AddSubMenuItemCheckbox`, while preserving persistence, checkbox state, and
refresh behavior. Verify the Linux build and existing tray tests.

## Verification

`go test ./internal/tui ./internal/tray ./internal/config`

`go build ./... && go vet ./...`

## Atomic commit

`fix: repair provider visibility controls`
