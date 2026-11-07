package metadata

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"time"
)

// CredentialFile maps a JIT credential's metadata path to its tmpfs filename.
type CredentialFile struct {
	// RemoteName is the path segment after "credentials/" in the request.
	RemoteName string
	// LocalName is the filename in the credential tmpfs. The image entrypoint
	// polls for this bare name and installs it with a leading dot (ADR-002).
	LocalName string
}

// JITCredentialFiles lists the JIT credential files GARM serves. A wrong name would
// silently break run.sh's direct boot, which does not run config.sh.
var JITCredentialFiles = []CredentialFile{
	{RemoteName: "runner", LocalName: "runner"},
	{RemoteName: "credentials", LocalName: "credentials"},
	{RemoteName: "credentials_rsaparams", LocalName: "credentials_rsaparams"},
}

// RegistrationTokenFile is the tmpfs filename of the non-JIT registration token,
// which the image entrypoint passes to config.sh (ADR-002).
const RegistrationTokenFile = "registration-token"

// registrationTokenPath is the metadata path of the non-JIT registration token.
const registrationTokenPath = "runner-registration-token"

// CredentialFileContent is one fetched credential file, held only in memory.
type CredentialFileContent struct {
	Name  string
	Bytes []byte
}

// FetchJITCredentials fetches every JIT credential file. If any fetch fails it
// returns an error and no partial set.
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

// FetchRegistrationToken fetches the non-JIT registration token, in the same form
// as a JIT file so both share one delivery path (ADR-002).
func (c *Client) FetchRegistrationToken(ctx context.Context) (CredentialFileContent, error) {
	b, err := c.get(ctx, registrationTokenPath)
	if err != nil {
		return CredentialFileContent{}, fmt.Errorf("failed to fetch runner registration token: %w", err)
	}
	// The service may add a trailing newline, which config.sh must not see.
	return CredentialFileContent{Name: RegistrationTokenFile, Bytes: bytes.TrimSpace(b)}, nil
}

// TarArchive packs files into a tar stream for `tar -x` in the credential tmpfs,
// run through `docker exec` because `docker cp` cannot write a running container's
// user tmpfs. It builds the archive in memory to keep credentials off host disk
// (ADR-002), and fixes ModTime so the output is deterministic.
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
