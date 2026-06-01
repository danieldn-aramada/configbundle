package controller

import (
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigsig "github.com/sigstore/sigstore/pkg/signature"
	"oras.land/oras-go/v2/registry/remote"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/armada/configbundle/bundle"
)

// HTTPOCIClient implements OCIClient using oras-go for OCI pulls and
// cosign for signature verification.
type HTTPOCIClient struct {
	registryURL   string
	cosignKeyPath string
}

// NewHTTPOCIClient returns an OCIClient configured for the given registry and cosign key.
func NewHTTPOCIClient(registryURL, cosignKeyPath string) *HTTPOCIClient {
	return &HTTPOCIClient{
		registryURL:   registryURL,
		cosignKeyPath: cosignKeyPath,
	}
}

// Pull fetches the OCI artifact at ref using oras-go, cosign-verifies it (if a key path
// is configured), and returns the extracted layers.
//
// ref must be in the form "host:port/repo:tag" or "host:port/repo@sha256:...".
// cosign verification is skipped when cosignKeyPath is empty — use this for local
// development against Zot without a signed artifact.
func (c *HTTPOCIClient) Pull(ctx context.Context, ref string) (*OCIArtifact, error) {
	logger := log.FromContext(ctx).WithName("oci-client")

	repoStr, tag, err := splitRef(ref)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", ref, err)
	}

	repo, err := remote.NewRepository(repoStr)
	if err != nil {
		return nil, fmt.Errorf("create repository client for %q: %w", repoStr, err)
	}
	repo.PlainHTTP = true // Zot runs plain HTTP in dev; TLS is a deployment concern

	// Fetch the manifest by tag/digest to get the canonical descriptor.
	desc, rc, err := repo.FetchReference(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("fetch reference %q: %w", ref, err)
	}
	manifestBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	logger.V(1).Info("fetched manifest", "ref", ref, "digest", desc.Digest.String())

	// cosign verify — skip when no key is configured (local dev without signed artifacts).
	if c.cosignKeyPath == "" {
		logger.Info("cosign verification skipped: no COSIGN_PUBLIC_KEY_PATH set")
	} else {
		if err := c.verifySignature(ctx, ref); err != nil {
			return nil, fmt.Errorf("cosign verify %s: %w", ref, err)
		}
		logger.V(1).Info("cosign signature verified", "ref", ref)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal OCI manifest: %w", err)
	}

	// Extract layers by media type.
	artifact := &OCIArtifact{Digest: desc.Digest.String()}
	for _, layerDesc := range manifest.Layers {
		layerRC, err := repo.Blobs().Fetch(ctx, layerDesc)
		if err != nil {
			return nil, fmt.Errorf("fetch layer %s: %w", layerDesc.MediaType, err)
		}
		data, err := io.ReadAll(layerRC)
		layerRC.Close()
		if err != nil {
			return nil, fmt.Errorf("read layer %s: %w", layerDesc.MediaType, err)
		}
		switch layerDesc.MediaType {
		case bundle.MediaTypeManifest:
			artifact.Manifest = data
		case bundle.MediaTypeData:
			artifact.Data = data
		case bundle.MediaTypeSchema:
			artifact.Schema = data
		default:
			logger.V(1).Info("ignoring unknown layer", "mediaType", layerDesc.MediaType)
		}
	}

	if artifact.Manifest == nil {
		return nil, fmt.Errorf("artifact %s has no %s layer", ref, bundle.MediaTypeManifest)
	}

	return artifact, nil
}

// verifySignature cosign-verifies the artifact at ref against the configured public key.
// Operates fully air-gapped: no Rekor transparency log, no CT log.
func (c *HTTPOCIClient) verifySignature(ctx context.Context, ref string) error {
	pubKeyPEM, err := os.ReadFile(c.cosignKeyPath)
	if err != nil {
		return fmt.Errorf("read public key %s: %w", c.cosignKeyPath, err)
	}

	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(pubKeyPEM)
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}

	verifier, err := sigsig.LoadVerifier(pubKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("load verifier: %w", err)
	}

	// name.Insecure allows plain-HTTP registries (Zot in dev).
	imgRef, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	co := &cosign.CheckOpts{
		SigVerifier: verifier,
		IgnoreTlog:  true, // air-gapped: no Rekor
		IgnoreSCT:   true, // air-gapped: no CT log
	}

	if _, _, err := cosign.VerifyImageSignatures(ctx, imgRef, co); err != nil {
		return err
	}
	return nil
}

// splitRef splits "host:port/repo:tag" into ("host:port/repo", "tag").
// If no tag is present, returns ("host:port/repo", "latest").
func splitRef(ref string) (string, string, error) {
	if ref == "" {
		return "", "", fmt.Errorf("empty reference")
	}
	// Find the last colon that appears after the last slash — that's the tag separator.
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon <= lastSlash {
		// No tag (or colon is part of host:port only).
		return ref, "latest", nil
	}
	// Check if what follows the colon looks like a digest (sha256:...)
	// In that case the entire "sha256:abc..." portion is the tag/reference.
	suffix := ref[lastColon+1:]
	if strings.Contains(suffix, ":") {
		// Digest reference: "host/repo@sha256:abc" — treat the whole thing as the tag.
		at := strings.LastIndex(ref, "@")
		if at >= 0 {
			return ref[:at], ref[at+1:], nil
		}
	}
	return ref[:lastColon], suffix, nil
}
