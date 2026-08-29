// Fences the vendored agent skills (and their example .go files) off from the
// parent module so `go build ./...` at the repo root does not compile them.
module ignore.local/agent-skills

go 1.25.0
