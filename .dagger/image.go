package main

import (
	"context"
	"dagger/dagger/internal/dagger"
	"fmt"
	"strings"
)

// koBuildWithConfig builds a container image using ko, respecting .ko.yaml configuration.
// It uses --bare to publish directly to the repo URL without import-path suffixes.
func (m *Dagger) koBuildWithConfig(
	ctx context.Context,
	src *dagger.Directory,
	repo string,
	buildArg string,
	koVersion string,
	push string,
	tokenName string,
	token *dagger.Secret,
	tag string,
) (string, error) {
	ctr := dag.Container().
		From(fmt.Sprintf("ghcr.io/ko-build/ko:%s", koVersion)).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src").
		WithEnvVariable("KO_DOCKER_REPO", repo)

	// Configure registry auth when pushing to ghcr.io
	if token != nil && push == "true" && strings.Contains(repo, "ghcr.io") {
		ctr = ctr.
			WithSecretVariable(tokenName, token).
			WithExec([]string{"sh", "-c",
				fmt.Sprintf("echo $%s | ko login ghcr.io --username=token --password-stdin", tokenName)})
	}

	koArgs := []string{"ko", "build", fmt.Sprintf("--push=%s", push), "--sbom=none", "--bare"}
	if tag != "" {
		koArgs = append(koArgs, fmt.Sprintf("--tags=%s", tag))
	}
	koArgs = append(koArgs, buildArg)
	ctr = ctr.WithExec(koArgs)

	stdout, err := ctr.Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// BuildImage builds a container image using ko and optionally scans it with Trivy
func (m *Dagger) BuildImage(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="ko.local"
	repo string,
	// +optional
	// +default="."
	buildArg string,
	// +optional
	// +default="v0.18.1"
	koVersion string,
	// +optional
	// +default="true"
	push string,
	// +optional
	// +default="GITHUB_TOKEN"
	tokenName string,
	// +optional
	token *dagger.Secret,
	// +optional
	// +default=""
	tag string,
	// +optional
	// +default=false
	scan bool,
	// +optional
	// +default="HIGH,CRITICAL"
	scanSeverity string,
) (string, error) {
	imageRef, err := m.koBuildWithConfig(
		ctx,
		src,
		repo,
		buildArg,
		koVersion,
		push,
		tokenName,
		token,
		tag,
	)

	if err != nil {
		return "", err
	}

	// Optionally scan the image after build
	if scan && push == "true" {
		scanReport, scanErr := m.ScanImage(ctx, imageRef, nil, nil, scanSeverity, "0.64.1")
		if scanErr != nil {
			return imageRef, scanErr
		}
		// Materialize the scan by getting its contents (forces execution)
		_, contentErr := scanReport.Contents(ctx)
		if contentErr != nil {
			return imageRef, contentErr
		}
	}

	return imageRef, nil
}

// ScanImage scans a container image for vulnerabilities using Trivy
func (m *Dagger) ScanImage(
	ctx context.Context,
	imageRef string, // Fully qualified image reference (e.g., "ttl.sh/my-repo:1.0.0")
	// +optional
	registryUser *dagger.Secret,
	// +optional
	registryPassword *dagger.Secret,
	// +optional
	// +default="HIGH,CRITICAL"
	severity string,
	// +optional
	// +default="0.64.1"
	trivyVersion string,
) (*dagger.File, error) {

	return dag.Trivy().ScanImage(
		imageRef,
		dagger.TrivyScanImageOpts{
			RegistryUser:     registryUser,
			RegistryPassword: registryPassword,
			Severity:         severity,
			TrivyVersion:     trivyVersion,
		}), nil
}
