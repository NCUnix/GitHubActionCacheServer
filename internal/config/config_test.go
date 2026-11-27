package config

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ADDR", "")
	t.Setenv("API_BASE_URL", "")
	t.Setenv("DEFAULT_ACTIONS_RESULTS_URL", "")
	t.Setenv("DOWNLOAD_IDLE_TIMEOUT_MS", "")
	t.Setenv("ACTIONS_TOKEN_ISSUER", "")
	t.Setenv("GITHUB_ACTIONS_TOKEN_ISSUER", "")
	t.Setenv("ACTIONS_TOKEN_JWKS_URL", "")
	t.Setenv("GITHUB_ACTIONS_TOKEN_JWKS_URL", "")
	t.Setenv("ACTIONS_TOKEN_AUDIENCE", "")
	t.Setenv("SKIP_TOKEN_VALIDATION", "")
	t.Setenv("UNSAFE_ALLOW_DIRECT_DOWNLOADS_WITHOUT_TOKEN_VALIDATION", "")
	t.Setenv("DB_DRIVER", "")
	t.Setenv("DB_SQLITE_PATH", "")
	t.Setenv("DB_POSTGRES_URL", "")
	t.Setenv("DB_POSTGRES_SSLMODE", "")
	t.Setenv("DB_MYSQL_TLS", "")
	t.Setenv("STORAGE_DRIVER", "")
	t.Setenv("STORAGE_FILESYSTEM_PATH", "")
	t.Setenv("STORAGE_FILESYSTEM_FSYNC", "")
	t.Setenv("STORAGE_S3_BUCKET", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("STORAGE_S3_FORCE_PATH_STYLE", "")
	t.Setenv("STORAGE_S3_KEY_PREFIX", "")
	t.Setenv("STORAGE_S3_DISABLE_STREAMING_UPLOADS", "")
	t.Setenv("STORAGE_S3_UPLOAD_PART_SIZE_BYTES", "")
	t.Setenv("STORAGE_S3_UPLOAD_CONCURRENCY", "")
	t.Setenv("STORAGE_S3_MULTIPART_ABORT_TIMEOUT", "")
	t.Setenv("ENABLE_DIRECT_DOWNLOADS", "")
	t.Setenv("DIRECT_DOWNLOAD_URL_TTL_SECONDS", "")
	t.Setenv("DOWNLOAD_URL_SIGNING_SECRET", "")
	t.Setenv("CACHE_MERGE_CONCURRENCY", "")
	t.Setenv("CACHE_MAX_SIZE_BYTES", "")
	t.Setenv("CACHE_FILESYSTEM_MAX_USAGE_PERCENT", "")
	t.Setenv("MANAGEMENT_API_KEY", "")
	t.Setenv("DISABLE_CLEANUP_JOBS", "")
	t.Setenv("CACHE_CLEANUP_OLDER_THAN_DAYS", "")
	t.Setenv("ORPHANED_STORAGE_GRACE_PERIOD_HOURS", "")
	t.Setenv("DEBUG", "")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, DefaultAddr, cfg.Server.Addr)
	require.Empty(t, cfg.Server.APIBaseURL)
	require.Equal(t, DefaultActionsResultsURL, cfg.Server.DefaultActionsResultsURL)
	require.Equal(t, 5*time.Minute, cfg.Server.DownloadIdleTimeout)
	require.Equal(t, DefaultTokenIssuer, cfg.Auth.TokenIssuer)
	require.Empty(t, cfg.Auth.TokenJWKSURL)
	require.Nil(t, cfg.Auth.TokenAudience)
	require.False(t, cfg.Auth.SkipTokenValidation)
	require.Equal(t, "sqlite", cfg.DB.Driver)
	require.Equal(t, ".data/sqlite.db", cfg.DB.SQLitePath)
	require.Equal(t, DefaultDBPostgresSSLMode, cfg.DB.PostgresSSLMode)
	require.Equal(t, DefaultDBMySQLTLS, cfg.DB.MySQLTLS)
	require.Equal(t, "filesystem", cfg.Storage.Driver)
	require.Equal(t, ".data/storage/filesystem", cfg.Storage.FilesystemPath)
	require.True(t, cfg.Storage.FilesystemFsync)
	require.Equal(t, "us-east-1", cfg.Storage.S3Region)
	require.True(t, cfg.Storage.S3ForcePathStyle)
	require.Equal(t, "gh-actions-cache", cfg.Storage.S3KeyPrefix)
	require.False(t, cfg.Storage.S3DisableStreamingUploads)
	require.Equal(t, int64(DefaultS3UploadPartSizeBytes), cfg.Storage.S3UploadPartSizeBytes)
	require.False(t, cfg.Storage.S3UploadPartSizeBytesConfigured)
	require.Equal(t, DefaultS3UploadConcurrency, cfg.Storage.S3UploadConcurrency)
	require.Equal(t, DefaultS3MultipartAbortTimeout, cfg.Storage.S3MultipartAbortTimeout)
	require.False(t, cfg.Cache.EnableDirectDownloads)
	require.Equal(t, 10*time.Minute, cfg.Cache.DirectDownloadURLTTL)
	require.Empty(t, cfg.Cache.DownloadURLSigningSecret)
	require.Equal(t, DefaultMergeConcurrency, cfg.Cache.MergeConcurrency)
	require.False(t, cfg.Cache.MaxSizeBytesConfigured)
	require.Zero(t, cfg.Cache.MaxSizeBytes)
	require.Equal(t, float64(DefaultFilesystemMaxUsagePercent), cfg.Cache.FilesystemMaxUsagePercent)
	require.Empty(t, cfg.Management.APIKey)
	require.False(t, cfg.Cleanup.Disabled)
	require.Equal(t, DefaultCacheOlderThanDays, cfg.Cleanup.CacheOlderThanDays)
	require.Equal(t, 24*time.Hour, cfg.Cleanup.OrphanedStorageGracePeriod)
	require.False(t, cfg.Debug)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("ADDR", ":8080")
	t.Setenv("API_BASE_URL", "https://cache.example")
	t.Setenv("DEFAULT_ACTIONS_RESULTS_URL", "https://results.example")
	t.Setenv("ACTIONS_TOKEN_ISSUER", "https://issuer.example")
	t.Setenv("GITHUB_ACTIONS_TOKEN_ISSUER", "https://legacy-issuer.example")
	t.Setenv("ACTIONS_TOKEN_JWKS_URL", "https://keys.example/jwks")
	t.Setenv("GITHUB_ACTIONS_TOKEN_JWKS_URL", "https://legacy-keys.example/jwks")
	t.Setenv("SKIP_TOKEN_VALIDATION", "true")
	t.Setenv("UNSAFE_ALLOW_DIRECT_DOWNLOADS_WITHOUT_TOKEN_VALIDATION", "true")
	t.Setenv("DB_DRIVER", "postgres")
	t.Setenv("DB_POSTGRES_URL", "postgres://example")
	t.Setenv("DB_POSTGRES_SSLMODE", "VERIFY-FULL")
	t.Setenv("DB_MYSQL_TLS", "TRUE")
	t.Setenv("STORAGE_DRIVER", "s3")
	t.Setenv("STORAGE_FILESYSTEM_FSYNC", "false")
	t.Setenv("STORAGE_S3_BUCKET", "cache-bucket")
	t.Setenv("AWS_REGION", "ap-east-1")
	t.Setenv("AWS_ENDPOINT_URL", "https://s3.example")
	t.Setenv("STORAGE_S3_FORCE_PATH_STYLE", "false")
	t.Setenv("STORAGE_S3_KEY_PREFIX", "custom-prefix")
	t.Setenv("STORAGE_S3_DISABLE_STREAMING_UPLOADS", "true")
	t.Setenv("STORAGE_S3_UPLOAD_PART_SIZE_BYTES", "10485760")
	t.Setenv("STORAGE_S3_UPLOAD_CONCURRENCY", "3")
	t.Setenv("STORAGE_S3_MULTIPART_ABORT_TIMEOUT", "45s")
	t.Setenv("ENABLE_DIRECT_DOWNLOADS", "true")
	t.Setenv("DOWNLOAD_URL_SIGNING_SECRET", "secret")
	t.Setenv("CACHE_MERGE_CONCURRENCY", "2")
	t.Setenv("CACHE_MAX_SIZE_BYTES", "1073741824")
	t.Setenv("CACHE_FILESYSTEM_MAX_USAGE_PERCENT", "82.5")
	t.Setenv("MANAGEMENT_API_KEY", "management-secret")
	t.Setenv("DISABLE_CLEANUP_JOBS", "true")
	t.Setenv("CACHE_CLEANUP_OLDER_THAN_DAYS", "30")
	t.Setenv("ORPHANED_STORAGE_GRACE_PERIOD_HOURS", "48")
	t.Setenv("DEBUG", "true")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, ":8080", cfg.Server.Addr)
	require.Equal(t, "https://cache.example", cfg.Server.APIBaseURL)
	require.Equal(t, "https://results.example", cfg.Server.DefaultActionsResultsURL)
	require.Equal(t, "https://issuer.example", cfg.Auth.TokenIssuer)
	require.Equal(t, "https://keys.example/jwks", cfg.Auth.TokenJWKSURL)
	require.True(t, cfg.Auth.SkipTokenValidation)
	require.Equal(t, "postgres", cfg.DB.Driver)
	require.Equal(t, "postgres://example", cfg.DB.PostgresURL)
	require.Equal(t, "verify-full", cfg.DB.PostgresSSLMode)
	require.Equal(t, "true", cfg.DB.MySQLTLS)
	require.Equal(t, "s3", cfg.Storage.Driver)
	require.False(t, cfg.Storage.FilesystemFsync)
	require.Equal(t, "cache-bucket", cfg.Storage.S3Bucket)
	require.Equal(t, "ap-east-1", cfg.Storage.S3Region)
	require.Equal(t, "https://s3.example", cfg.Storage.S3EndpointURL)
	require.False(t, cfg.Storage.S3ForcePathStyle)
	require.Equal(t, "custom-prefix", cfg.Storage.S3KeyPrefix)
	require.True(t, cfg.Storage.S3DisableStreamingUploads)
	require.Equal(t, int64(10*1024*1024), cfg.Storage.S3UploadPartSizeBytes)
	require.True(t, cfg.Storage.S3UploadPartSizeBytesConfigured)
	require.Equal(t, 3, cfg.Storage.S3UploadConcurrency)
	require.Equal(t, 45*time.Second, cfg.Storage.S3MultipartAbortTimeout)
	require.True(t, cfg.Cache.EnableDirectDownloads)
	require.Equal(t, "secret", cfg.Cache.DownloadURLSigningSecret)
	require.Equal(t, 2, cfg.Cache.MergeConcurrency)
	require.True(t, cfg.Cache.MaxSizeBytesConfigured)
	require.Equal(t, int64(1073741824), cfg.Cache.MaxSizeBytes)
	require.Equal(t, 82.5, cfg.Cache.FilesystemMaxUsagePercent)
	require.Equal(t, "management-secret", cfg.Management.APIKey)
	require.True(t, cfg.Cleanup.Disabled)
	require.Equal(t, 30, cfg.Cleanup.CacheOlderThanDays)
	require.Equal(t, 48*time.Hour, cfg.Cleanup.OrphanedStorageGracePeriod)
	require.True(t, cfg.Debug)
}

func TestLoadTokenIssuerConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		issuer        string
		legacyIssuer  string
		jwksURL       string
		legacyJWKSURL string
		wantIssuer    string
		wantJWKSURL   string
	}{
		{
			name:       "defaults leave JWKS URL to discovery",
			wantIssuer: DefaultTokenIssuer,
		},
		{
			name:       "canonical issuer trims trailing slashes",
			issuer:     "https://ghes.example///",
			wantIssuer: "https://ghes.example",
		},
		{
			name:         "legacy issuer",
			legacyIssuer: "https://legacy-ghes.example/",
			wantIssuer:   "https://legacy-ghes.example",
		},
		{
			name:         "canonical issuer takes precedence over legacy issuer",
			issuer:       "https://canonical.example",
			legacyIssuer: "https://legacy.example",
			wantIssuer:   "https://canonical.example",
		},
		{
			name:        "canonical JWKS URL",
			issuer:      "https://ghes.example/",
			jwksURL:     "https://keys.example/custom-jwks",
			wantIssuer:  "https://ghes.example",
			wantJWKSURL: "https://keys.example/custom-jwks",
		},
		{
			name:          "legacy JWKS URL",
			legacyJWKSURL: "https://legacy-keys.example/jwks",
			wantIssuer:    DefaultTokenIssuer,
			wantJWKSURL:   "https://legacy-keys.example/jwks",
		},
		{
			name:          "canonical JWKS URL takes precedence over legacy JWKS URL",
			jwksURL:       "https://keys.example/jwks",
			legacyJWKSURL: "https://legacy-keys.example/jwks",
			wantIssuer:    DefaultTokenIssuer,
			wantJWKSURL:   "https://keys.example/jwks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ACTIONS_TOKEN_ISSUER", tt.issuer)
			t.Setenv("GITHUB_ACTIONS_TOKEN_ISSUER", tt.legacyIssuer)
			t.Setenv("ACTIONS_TOKEN_JWKS_URL", tt.jwksURL)
			t.Setenv("GITHUB_ACTIONS_TOKEN_JWKS_URL", tt.legacyJWKSURL)

			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tt.wantIssuer, cfg.Auth.TokenIssuer)
			require.Equal(t, tt.wantJWKSURL, cfg.Auth.TokenJWKSURL)
		})
	}
}

func TestLoadTokenAudience(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "single audience", value: "https://github.com/my-org", want: []string{"https://github.com/my-org"}},
		{name: "multiple audiences", value: "a,b", want: []string{"a", "b"}},
		{name: "trims spaces and drops empty entries", value: " a , ,b ", want: []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ACTIONS_TOKEN_AUDIENCE", tt.value)

			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Auth.TokenAudience)
		})
	}
}

func TestLoadRejectsTokenAudienceWithoutEntries(t *testing.T) {
	for _, value := range []string{",", ", ,", " "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ACTIONS_TOKEN_AUDIENCE", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.ErrorContains(t, err, "ACTIONS_TOKEN_AUDIENCE must list at least one audience")
			require.ErrorContains(t, err, value)
		})
	}
}

func TestLoadPreservesS3UploadPartSizeBelowMinimumForValidation(t *testing.T) {
	t.Setenv("STORAGE_S3_UPLOAD_PART_SIZE_BYTES", "1048576")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, int64(1048576), cfg.Storage.S3UploadPartSizeBytes)
	require.True(t, cfg.Storage.S3UploadPartSizeBytesConfigured)
}

func TestLoadRejectsMalformedS3UploadPartSize(t *testing.T) {
	tests := []string{"10MiB", "abc", "9223372036854775808"}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			t.Setenv("STORAGE_S3_UPLOAD_PART_SIZE_BYTES", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "STORAGE_S3_UPLOAD_PART_SIZE_BYTES")
			require.Contains(t, err.Error(), value)
		})
	}
}

func TestLoadRejectsInvalidDatabaseTLSModes(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{key: "DB_POSTGRES_SSLMODE", value: "sometimes"},
		{key: "DB_MYSQL_TLS", value: "required"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			require.ErrorContains(t, err, tt.key+" must be one of")
		})
	}
}

func TestLoadRejectsInvalidCapacityConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero byte budget", key: "CACHE_MAX_SIZE_BYTES", value: "0"},
		{name: "negative byte budget", key: "CACHE_MAX_SIZE_BYTES", value: "-1"},
		{name: "malformed byte budget", key: "CACHE_MAX_SIZE_BYTES", value: "1GiB"},
		{name: "zero filesystem percent", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "0"},
		{name: "negative filesystem percent", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "-1"},
		{name: "filesystem percent above one hundred", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "100.1"},
		{name: "filesystem percent NaN", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "NaN"},
		{name: "filesystem percent infinity", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "+Inf"},
		{name: "malformed filesystem percent", key: "CACHE_FILESYSTEM_MAX_USAGE_PERCENT", value: "high"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CACHE_MAX_SIZE_BYTES", "")
			t.Setenv("CACHE_FILESYSTEM_MAX_USAGE_PERCENT", "")
			t.Setenv(tt.key, tt.value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.key)
			require.Contains(t, err.Error(), tt.value)
		})
	}
}

func TestLoadRejectsInvalidOrphanedStorageGracePeriod(t *testing.T) {
	for _, value := range []string{"0", "-1", "1.5", "forever", "2562048"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ORPHANED_STORAGE_GRACE_PERIOD_HOURS", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "ORPHANED_STORAGE_GRACE_PERIOD_HOURS")
			require.Contains(t, err.Error(), value)
		})
	}
}

func TestLoadAcceptsCleanupRetentionBounds(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{value: "0", want: 0},
		{value: strconv.Itoa(MaximumCacheOlderThanDays), want: MaximumCacheOlderThanDays},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("CACHE_CLEANUP_OLDER_THAN_DAYS", tt.value)

			cfg, err := Load()

			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Cleanup.CacheOlderThanDays)
		})
	}
}

func TestLoadRejectsInvalidCleanupRetention(t *testing.T) {
	for _, value := range []string{"-1", strconv.Itoa(MaximumCacheOlderThanDays + 1), "1.5", "forever", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CACHE_CLEANUP_OLDER_THAN_DAYS", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "CACHE_CLEANUP_OLDER_THAN_DAYS")
			require.Contains(t, err.Error(), value)
		})
	}
}

func TestLoadDownloadIdleTimeout(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "0", want: 0},
		{value: "1500", want: 1500 * time.Millisecond},
		{value: strconv.Itoa(MaximumDownloadIdleTimeoutMS), want: MaximumDownloadIdleTimeoutMS * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("DOWNLOAD_IDLE_TIMEOUT_MS", tt.value)

			cfg, err := Load()

			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Server.DownloadIdleTimeout)
		})
	}
}

func TestLoadRejectsInvalidDownloadIdleTimeout(t *testing.T) {
	for _, value := range []string{"-1", "abc", "1.5", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DOWNLOAD_IDLE_TIMEOUT_MS", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "DOWNLOAD_IDLE_TIMEOUT_MS")
			require.Contains(t, err.Error(), value)
		})
	}
}

func TestLoadDirectDownloadURLTTL(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "1", want: time.Second},
		{value: "120", want: 2 * time.Minute},
		{value: strconv.Itoa(MaximumDirectDownloadURLTTLSeconds), want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("DIRECT_DOWNLOAD_URL_TTL_SECONDS", tt.value)

			cfg, err := Load()

			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Cache.DirectDownloadURLTTL)
		})
	}
}

func TestLoadRejectsInvalidDirectDownloadURLTTL(t *testing.T) {
	for _, value := range []string{"0", "3601", "-1", "abc", "1.5"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DIRECT_DOWNLOAD_URL_TTL_SECONDS", value)

			cfg, err := Load()

			require.Zero(t, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "DIRECT_DOWNLOAD_URL_TTL_SECONDS")
			require.Contains(t, err.Error(), value)
		})
	}
}

func TestLoadDirectDownloadsWithSkippedTokenValidation(t *testing.T) {
	tests := []struct {
		name                  string
		enableDirectDownloads bool
		skipTokenValidation   bool
		allowUnsafe           bool
		wantErr               bool
	}{
		{name: "both enabled without override", enableDirectDownloads: true, skipTokenValidation: true, wantErr: true},
		{name: "both enabled with override", enableDirectDownloads: true, skipTokenValidation: true, allowUnsafe: true},
		{name: "only direct downloads", enableDirectDownloads: true},
		{name: "only skipped token validation", skipTokenValidation: true},
		{name: "override alone", allowUnsafe: true},
		{name: "override with only direct downloads", enableDirectDownloads: true, allowUnsafe: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENABLE_DIRECT_DOWNLOADS", strconv.FormatBool(tt.enableDirectDownloads))
			t.Setenv("SKIP_TOKEN_VALIDATION", strconv.FormatBool(tt.skipTokenValidation))
			t.Setenv("UNSAFE_ALLOW_DIRECT_DOWNLOADS_WITHOUT_TOKEN_VALIDATION", strconv.FormatBool(tt.allowUnsafe))

			cfg, err := Load()

			if tt.wantErr {
				require.Zero(t, cfg)
				require.Error(t, err)
				require.Contains(t, err.Error(), "ENABLE_DIRECT_DOWNLOADS")
				require.Contains(t, err.Error(), "SKIP_TOKEN_VALIDATION")
				require.Contains(t, err.Error(), "UNSAFE_ALLOW_DIRECT_DOWNLOADS_WITHOUT_TOKEN_VALIDATION")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.enableDirectDownloads, cfg.Cache.EnableDirectDownloads)
			require.Equal(t, tt.skipTokenValidation, cfg.Auth.SkipTokenValidation)
		})
	}
}

func TestLoadRequiresSigningSecretForSharedDatabases(t *testing.T) {
	tests := []struct {
		name    string
		driver  string
		secret  string
		wantErr bool
	}{
		{name: "postgres without secret", driver: "postgres", wantErr: true},
		{name: "mysql without secret", driver: "mysql", wantErr: true},
		{name: "postgres with secret", driver: "postgres", secret: "secret"},
		{name: "sqlite without secret", driver: "sqlite"},
		{name: "default driver without secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_DRIVER", tt.driver)
			t.Setenv("DOWNLOAD_URL_SIGNING_SECRET", tt.secret)

			cfg, err := Load()

			if tt.wantErr {
				require.Zero(t, cfg)
				require.Error(t, err)
				require.Contains(t, err.Error(), "DOWNLOAD_URL_SIGNING_SECRET")
				require.Contains(t, err.Error(), "DB_DRIVER="+tt.driver)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.secret, cfg.Cache.DownloadURLSigningSecret)
		})
	}
}
