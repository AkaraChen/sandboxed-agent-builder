package api

import (
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

func logLines(texts ...string) []domain.LogEntry {
	var out []domain.LogEntry
	for _, t := range texts {
		out = append(out, domain.LogEntry{Time: time.Now(), Stage: domain.StageBuild, Stream: "buildkit", Text: t})
	}
	return out
}

func failed() domain.Outcome {
	return domain.Outcome{Status: domain.StatusFailed, Failure: &domain.Failure{Stage: domain.StageBuild, Code: domain.CodeBuildFailed, Message: "x"}}
}
