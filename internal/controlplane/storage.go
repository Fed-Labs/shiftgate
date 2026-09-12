package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/objectstore"
)

// storage.go: the hosted-checkpoint-storage broker. The platform runs one
// S3-compatible store; this service holds the single parent credential in
// memory, counts each organization's footprint by listing its prefix, and
// mints per-organization, prefix-scoped, short-lived credentials through STS
// AssumeRole. Agents never see the parent credential; the control plane never
// sees plaintext checkpoint data or workload keys — the bucket holds AEAD
// ciphertext only.

// storageUsageSnapshot is one organization's counted footprint and the moment
// it was counted.
type storageUsageSnapshot struct {
	bytes int64
	at    time.Time
}

type storageService struct {
	configuration config.StorageConfig
	// lister counts usage with the parent credential. It is read-only in
	// practice: no state directory, so multipart methods refuse to run.
	lister *objectstore.S3
	// assumeRole mints scoped credentials. It is a field so tests can replace
	// the STS round trip; production binds objectstore.AssumeRole with the
	// parent credential closed over.
	assumeRole func(ctx context.Context, policy string, ttl time.Duration) (objectstore.STSCredentials, error)

	mu    sync.Mutex
	usage map[string]storageUsageSnapshot
}

// newStorageService builds the broker for a validated storage configuration.
// It returns nil (not an error) when hosting is disabled — every caller treats
// nil as "not configured", which is the stable external contract.
func newStorageService(configuration config.StorageConfig) (*storageService, error) {
	if !configuration.Enabled {
		return nil, nil
	}
	lister, err := objectstore.OpenS3(objectstore.S3Config{
		Endpoint:        configuration.Endpoint,
		Region:          configuration.Region,
		Bucket:          configuration.Bucket,
		AccessKeyID:     configuration.AccessKeyID,
		SecretAccessKey: configuration.SecretAccessKey,
		// The hosted store is addressed path-style: MinIO (and every
		// S3-compatible service this deployment model targets) serves
		// bucket-in-path, not wildcard virtual hosts.
		ForcePathStyle: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open hosted storage broker: %w", err)
	}
	endpoint := configuration.Endpoint
	region := configuration.Region
	accessKeyID := configuration.AccessKeyID
	secretAccessKey := configuration.SecretAccessKey
	service := &storageService{
		configuration: configuration,
		lister:        lister,
		usage:         make(map[string]storageUsageSnapshot),
	}
	service.assumeRole = func(ctx context.Context, policy string, ttl time.Duration) (objectstore.STSCredentials, error) {
		return objectstore.AssumeRole(ctx, endpoint, region, accessKeyID, secretAccessKey, policy, ttl, nil)
	}
	return service, nil
}

// storagePrefix is the key prefix one organization's mirrored objects live
// under, without the trailing slash ("org/org_123").
func storagePrefix(organizationID string) string {
	return "org/" + organizationID
}

// sessionPolicy is the inline policy stamped onto every credential this broker
// mints: object reads, writes, deletes, and multipart operations restricted to
// the organization's own prefix, and bucket listings that can only see that
// prefix. Marshaling a map keeps the keys sorted, so the policy is stable and
// comparable across issuances.
func (service *storageService) sessionPolicy(organizationID string) string {
	prefix := storagePrefix(organizationID)
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect": "Allow",
				"Action": []string{
					"s3:GetObject", "s3:PutObject", "s3:DeleteObject",
					"s3:ListMultipartUploadParts", "s3:AbortMultipartUpload",
				},
				"Resource": []string{fmt.Sprintf("arn:aws:s3:::%s/%s/*", service.configuration.Bucket, prefix)},
			},
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:ListBucket"},
				"Resource": []string{fmt.Sprintf("arn:aws:s3:::%s", service.configuration.Bucket)},
				"Condition": map[string]any{
					"StringLike": map[string][]string{"s3:prefix": {prefix + "/", prefix + "/*"}},
				},
			},
		},
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		// A map of strings and slices cannot fail to marshal; if it somehow
		// does, refuse to mint credentials rather than sign an empty policy.
		return ""
	}
	return string(encoded)
}

// snapshot returns the last counted footprint for an organization.
func (service *storageService) snapshot(organizationID string) (storageUsageSnapshot, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	seen, ok := service.usage[organizationID]
	return seen, ok
}

// reconcileOrganization recounts one organization's prefix in the bucket —
// the only source of usage truth; agents never report their own usage — and
// caches the snapshot. It does not write the database; the caller decides
// whether this is a background pass or a synchronous status refresh.
func (service *storageService) reconcileOrganization(ctx context.Context, organizationID string) (storageUsageSnapshot, error) {
	var total int64
	err := service.lister.ListPrefix(ctx, storagePrefix(organizationID)+"/", func(info objectstore.ObjectInfo) error {
		total += info.Size
		return nil
	})
	if err != nil {
		return storageUsageSnapshot{}, err
	}
	seen := storageUsageSnapshot{bytes: total, at: time.Now().UTC()}
	service.mu.Lock()
	service.usage[organizationID] = seen
	service.mu.Unlock()
	return seen, nil
}

// stale reports whether the cached snapshot for an organization is older than
// the reconcile interval, or was never taken.
func (service *storageService) stale(organizationID string) bool {
	seen, ok := service.snapshot(organizationID)
	if !ok {
		return true
	}
	return time.Since(seen.at) >= service.configuration.ReconcileInterval
}

// reconcileStorageUsage recounts every organization and persists the result:
// entitlements.used_storage_bytes (the quota the broker enforces) and one
// checkpoint_storage gauge row for today. A failing organization is logged and
// skipped — one unreachable prefix must not blind the pass to the rest.
func (server *Server) reconcileStorageUsage(ctx context.Context) {
	if server.storage == nil {
		return
	}
	organizationIDs, err := server.database.OrganizationIDs(ctx)
	if err != nil {
		server.logger.Warn("storage reconcile could not list organizations", "error", err)
		return
	}
	for _, organizationID := range organizationIDs {
		seen, err := server.storage.reconcileOrganization(ctx, organizationID)
		if err != nil {
			server.logger.Warn("storage reconcile failed", "organization_id", organizationID, "error", err)
			continue
		}
		if err := server.database.UpdateStorageUsage(ctx, organizationID, seen.bytes); err != nil {
			server.logger.Warn("storage reconcile could not persist usage", "organization_id", organizationID, "error", err)
			continue
		}
		day := seen.at.UTC().Truncate(24 * time.Hour)
		if err := server.database.UpsertDailyUsage(ctx, database.UsageRecord{
			OrganizationID: organizationID,
			Kind:           "checkpoint_storage",
			Quantity:       seen.bytes,
			PeriodStart:    day,
			PeriodEnd:      day,
			Metadata:       json.RawMessage(`{"source":"reconciler"}`),
		}); err != nil {
			server.logger.Warn("storage reconcile could not record usage", "organization_id", organizationID, "error", err)
		}
	}
}

// startStorageReconciler runs the usage recount on the configured interval
// until ctx ends. Hosting disabled means no loop at all.
func (server *Server) startStorageReconciler(ctx context.Context) {
	if server.storage == nil {
		return
	}
	interval := server.storage.configuration.ReconcileInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// The pass runs on a detached context: a recount cut short by
				// request shutdown would leave half-persisted state, and the
				// next tick would simply redo it.
				reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				server.reconcileStorageUsage(reconcileCtx)
				cancel()
			}
		}
	}()
}
