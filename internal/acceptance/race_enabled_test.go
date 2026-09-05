//go:build race

package acceptance

// A race-enabled acceptance run must instrument the actual downstream tests too.
const childRace = true
