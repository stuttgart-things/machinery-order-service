package main

import (
	"context"
	"dagger/dagger/internal/dagger"
	"fmt"
)

// Lint runs the linter on the provided source code and returns a report file
func (m *Dagger) Lint(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="500s"
	timeout string,
) (*dagger.File, error) {
	lintContainer := dag.Go().Lint(
		src,
		dagger.GoLintOpts{Timeout: timeout},
	)

	out, err := lintContainer.Stdout(ctx)
	if err != nil {
		// If available, include both stderr and stdout from the failed exec
		if exitErr, ok := err.(*dagger.ExecError); ok {
			out = fmt.Sprintf("%s\n%s", exitErr.Stderr, exitErr.Stdout)
		}
	}

	// Return the lint output as a file for easy export
	reportDir := dag.Directory().WithNewFile("/lint-report.txt", out)
	return reportDir.File("/lint-report.txt"), nil
}
