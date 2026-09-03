package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"shift.dev/shift/internal/persistence"
)

// maxFeedBytes bounds an update feed. A feed is metadata; anything larger is a
// misconfigured endpoint and is refused before it is parsed.
const maxFeedBytes = 4 << 20

// maxArtifactBytes bounds one artifact download regardless of what the release
// claims, so a hostile feed cannot fill the disk by advertising an enormous size.
const maxArtifactBytes = int64(2) << 30

// FeedSource produces the release feed. It is an interface because operators
// publish feeds over HTTPS in the ordinary case and from a file on disk when the
// fleet is air-gapped; both are real sources, and neither weakens verification,
// which happens on the signed release rather than on the transport.
type FeedSource interface {
	Feed(ctx context.Context) (Feed, error)
}

// Fetcher retrieves one artifact's published bytes.
type Fetcher interface {
	Fetch(ctx context.Context, artifact Artifact, into io.Writer) error
}

// HTTPFeedSource reads a feed document over HTTPS.
type HTTPFeedSource struct {
	url    string
	client *http.Client
}

// NewHTTPFeedSource builds a feed source. The URL must be absolute HTTPS: an
// update feed is never fetched over plaintext, even though every release in it is
// signed.
func NewHTTPFeedSource(feedURL string, timeout time.Duration, client *http.Client) (*HTTPFeedSource, error) {
	parsed, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil {
		return nil, fmt.Errorf("parse update feed url: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("update feed url must be an absolute https url")
	}
	if client == nil {
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPFeedSource{url: parsed.String(), client: client}, nil
}

// Feed fetches and validates the feed document.
func (source *HTTPFeedSource) Feed(ctx context.Context) (Feed, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, nil)
	if err != nil {
		return Feed{}, fmt.Errorf("build update feed request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := source.client.Do(request)
	if err != nil {
		return Feed{}, fmt.Errorf("fetch update feed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxFeedBytes))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return Feed{}, fmt.Errorf("update feed responded %s", response.Status)
	}
	var feed Feed
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxFeedBytes))
	if err := decoder.Decode(&feed); err != nil {
		return Feed{}, fmt.Errorf("decode update feed: %w", err)
	}
	if err := feed.Validate(); err != nil {
		return Feed{}, err
	}
	return feed, nil
}

// FileFeedSource reads a feed document from disk, which is how an air-gapped
// fleet is updated: an operator copies in a signed feed and the same
// verification runs against it.
type FileFeedSource struct {
	path string
}

// NewFileFeedSource builds a feed source over a local file.
func NewFileFeedSource(path string) (*FileFeedSource, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("a file feed source requires a path")
	}
	return &FileFeedSource{path: path}, nil
}

// Feed reads and validates the feed document.
func (source *FileFeedSource) Feed(context.Context) (Feed, error) {
	var feed Feed
	if err := persistence.ReadJSON(source.path, &feed); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Feed{}, fmt.Errorf("update feed %s does not exist", source.path)
		}
		return Feed{}, err
	}
	if err := feed.Validate(); err != nil {
		return Feed{}, err
	}
	return feed, nil
}

// HTTPFetcher downloads artifacts over HTTPS.
type HTTPFetcher struct {
	client *http.Client
}

// NewHTTPFetcher builds an artifact fetcher. A caller may supply its own client,
// which is how a deployment pins a certificate pool or routes through a proxy.
func NewHTTPFetcher(timeout time.Duration, client *http.Client) *HTTPFetcher {
	if client == nil {
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPFetcher{client: client}
}

// Fetch streams one artifact. The transfer is bounded by the size the signed
// release recorded, and a body that does not match that size exactly is an
// error: the digest check that follows must run on the bytes that were promised.
func (fetcher *HTTPFetcher) Fetch(ctx context.Context, artifact Artifact, into io.Writer) error {
	if artifact.SizeBytes <= 0 || artifact.SizeBytes > maxArtifactBytes {
		return fmt.Errorf("artifact size %d is outside the accepted range", artifact.SizeBytes)
	}
	parsed, err := url.Parse(artifact.URL)
	if err != nil {
		return fmt.Errorf("parse artifact url: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("artifact url must be an absolute https url")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return fmt.Errorf("build artifact request: %w", err)
	}
	response, err := fetcher.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch artifact: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact download responded %s", response.Status)
	}
	if response.ContentLength >= 0 && response.ContentLength != artifact.SizeBytes {
		return fmt.Errorf("artifact reports %d bytes, the release records %d", response.ContentLength, artifact.SizeBytes)
	}
	written, err := io.Copy(into, io.LimitReader(response.Body, artifact.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if written != artifact.SizeBytes {
		return fmt.Errorf("artifact is %d bytes, the release records %d", written, artifact.SizeBytes)
	}
	return nil
}

// FileFetcher reads artifacts from local paths, which is how an air-gapped fleet
// is updated: an operator copies the artifacts onto the machine alongside the
// feed. Verification is identical to the networked case — the installer checks
// both digests of what it read — because the transport was never the trust
// boundary; the signature over the digest is.
type FileFetcher struct{}

// NewFileFetcher builds the local artifact fetcher.
func NewFileFetcher() *FileFetcher {
	return &FileFetcher{}
}

// Fetch copies one artifact's published bytes. The artifact URL must be an
// absolute file url; a size that disagrees with the signed release is refused
// before a single byte is copied, so a partially copied artifact set cannot be
// installed by mistake.
func (fetcher *FileFetcher) Fetch(_ context.Context, artifact Artifact, into io.Writer) error {
	if artifact.SizeBytes <= 0 || artifact.SizeBytes > maxArtifactBytes {
		return fmt.Errorf("artifact size %d is outside the accepted range", artifact.SizeBytes)
	}
	parsed, err := url.Parse(artifact.URL)
	if err != nil {
		return fmt.Errorf("parse artifact url: %w", err)
	}
	if parsed.Scheme != "file" || parsed.Host != "" || !path.IsAbs(parsed.Path) {
		return errors.New("a file artifact url must be an absolute file:// path")
	}
	file, err := os.Open(parsed.Path)
	if err != nil {
		return fmt.Errorf("open artifact %s: %w", parsed.Path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat artifact %s: %w", parsed.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact %s is not a regular file", parsed.Path)
	}
	if info.Size() != artifact.SizeBytes {
		return fmt.Errorf("artifact is %d bytes, the release records %d", info.Size(), artifact.SizeBytes)
	}
	written, err := io.CopyN(into, file, artifact.SizeBytes)
	if err != nil {
		return fmt.Errorf("read artifact %s: %w", parsed.Path, err)
	}
	if written != artifact.SizeBytes {
		return fmt.Errorf("artifact is %d bytes, the release records %d", written, artifact.SizeBytes)
	}
	return nil
}
