package metadata

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"time"
)

// CredentialFile pairs the metadata-service path segment used to fetch a
// JIT credential file with the on-disk filename the runner expects for it.
type CredentialFile struct {
	// RemoteName is the path segment after "credentials/" in the request
	// GET {metadata-url}/credentials/{RemoteName}. Note: no leading dot.
	RemoteName string
	// LocalName is the filename the bytes are written to inside the
	// runner's credential tmpfs. Note the leading dot: run.sh reads
	// .runner / .credentials / .credentials_rsaparams straight from the
	// working directory the entrypoint moves them into (ADR-002).
	LocalName string
}

// JITCredentialFiles is the single authoritative mapping of the three JIT
// credential files GARM's metadata service serves. Verified against GARM's
// GetJITConfigFile handler (which returns entries by their JitConfiguration
// map key) and the reference k8s provider's entrypoint, which fetches
// exactly credentials/runner, credentials/credentials, and
// credentials/credentials_rsaparams and writes them to .runner,
// .credentials, and .credentials_rsaparams (research.md §1.C, §2.A).
//
// Keeping the remote→local mapping in exactly one place is deliberate: a
// mismatch between the fetch path segment and the on-disk filename would
// silently break run.sh's direct (config.sh-less) boot.
var JITCredentialFiles = []CredentialFile{
	{RemoteName: "runner", LocalName: ".runner"},
	{RemoteName: "credentials", LocalName: ".credentials"},
	{RemoteName: "credentials_rsaparams", LocalName: ".credentials_rsaparams"},
}

// RegistrationTokenFile is the on-disk filename the non-JIT registration
// token is delivered as inside the credential tmpfs. The entrypoint reads
// it and runs `config.sh --token "$(cat …)"` (ADR-002 non-JIT fallback).
// It is a leading-dot name for consistency with the JIT files and to keep
// it out of casual directory listings inside the runner.
const RegistrationTokenFile = ".registration-token"

// registrationTokenPath is the metadata path for the classic, non-JIT
// runner registration token (research.md §1.C metadata router).
const registrationTokenPath = "runner-registration-token"

// CredentialFileContent is one fetched credential file: the on-disk name it
// should be written to and its raw bytes. The bytes live only in memory.
type CredentialFileContent struct {
	Name  string
	Bytes []byte
}

// FetchJITCredentials fetches all three JIT credential files. It returns an
// error and no partial set if any single file fails, so a caller never
// delivers an incomplete credential set into a container.
func (c *Client) FetchJITCredentials(ctx context.Context) ([]CredentialFileContent, error) {
	out := make([]CredentialFileContent, 0, len(JITCredentialFiles))
	for _, f := range JITCredentialFiles {
		b, err := c.get(ctx, "credentials/"+f.RemoteName)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch JIT credential %q: %w", f.RemoteName, err)
		}
		out = append(out, CredentialFileContent{Name: f.LocalName, Bytes: b})
	}
	return out, nil
}

// FetchRegistrationToken fetches the classic (non-JIT) runner registration
// token. It is returned as a single CredentialFileContent so the delivery
// path into the container is byte-for-byte identical to the JIT path
// (ADR-002: one uniform create → start → poll → docker cp mechanism).
func (c *Client) FetchRegistrationToken(ctx context.Context) (CredentialFileContent, error) {
	b, err := c.get(ctx, registrationTokenPath)
	if err != nil {
		return CredentialFileContent{}, fmt.Errorf("failed to fetch runner registration token: %w", err)
	}
	// The metadata service returns the bare token, sometimes with a
	// trailing newline; trim it so `config.sh --token "$(cat …)"` gets a
	// clean value.
	return CredentialFileContent{Name: RegistrationTokenFile, Bytes: bytes.TrimSpace(b)}, nil
}

// TarArchive packs credential files into an in-memory tar stream suitable
// for docker cp (CopyToContainer). Entry names are the plain LocalName with
// no leading path, so extracting the archive at the tmpfs mount point (the
// CopyToContainer dstPath) lands each file directly in that directory.
//
// The archive is built entirely in memory — no host-side temp file is ever
// created — which is the ADR-002 invariant that keeps credentials off host
// disk. ModTime is fixed to the Unix epoch so the output is deterministic
// (there is nothing time-sensitive about a credential file's mtime).
func TarArchive(files []CredentialFileContent) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.Name,
			Mode:     0o600,
			Size:     int64(len(f.Bytes)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %q: %w", f.Name, err)
		}
		if _, err := tw.Write(f.Bytes); err != nil {
			return nil, fmt.Errorf("failed to write tar body for %q: %w", f.Name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize credential tar: %w", err)
	}
	return &buf, nil
}
