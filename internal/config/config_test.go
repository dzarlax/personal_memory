package config

import (
	"bytes"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
)

func TestLoadOAuthConfigDefaultsResourceFromMemoryDomain(t *testing.T) {
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/personal-memory")
	t.Setenv("OAUTH_SCOPES", "")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("OAUTH_AUDIENCE", "")
	t.Setenv("OAUTH_AUTHORIZATION_SERVERS", "")

	cfg, err := loadOAuthConfig("example.com")
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.Enabled {
		t.Fatal("expected OAuth enabled")
	}
	if cfg.Resource != "https://mcp.example.com" {
		t.Fatalf("unexpected resource: %q", cfg.Resource)
	}
	if cfg.Audience != "https://mcp.example.com" {
		t.Fatalf("unexpected audience: %q", cfg.Audience)
	}
	if !reflect.DeepEqual(cfg.Scopes, []string{"memory:mcp"}) {
		t.Fatalf("unexpected scopes: %#v", cfg.Scopes)
	}
	if !reflect.DeepEqual(cfg.AuthorizationServers, []string{"https://auth.example.com/application/o/personal-memory"}) {
		t.Fatalf("unexpected authorization servers: %#v", cfg.AuthorizationServers)
	}
}

func TestLoadOAuthConfigCSV(t *testing.T) {
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_RESOURCE", "https://mcp.example.com")
	t.Setenv("OAUTH_AUDIENCE", "personal-memory")
	t.Setenv("OAUTH_SCOPES", "memory:read, memory:write")
	t.Setenv("OAUTH_AUTHORIZATION_SERVERS", "https://auth1.example.com, https://auth2.example.com")

	cfg, err := loadOAuthConfig("")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Audience != "personal-memory" {
		t.Fatalf("unexpected audience: %q", cfg.Audience)
	}
	if !reflect.DeepEqual(cfg.Scopes, []string{"memory:read", "memory:write"}) {
		t.Fatalf("unexpected scopes: %#v", cfg.Scopes)
	}
	if !reflect.DeepEqual(cfg.AuthorizationServers, []string{"https://auth1.example.com", "https://auth2.example.com"}) {
		t.Fatalf("unexpected authorization servers: %#v", cfg.AuthorizationServers)
	}
}

func TestLoadOAuthConfigAdditionalIssuersArePublished(t *testing.T) {
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/chatgpt/")
	t.Setenv("OAUTH_ADDITIONAL_ISSUERS", "https://auth.example.com/application/o/gemini/")
	t.Setenv("OAUTH_AUTHORIZATION_SERVERS", "")

	cfg, err := loadOAuthConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AdditionalIssuers, []string{"https://auth.example.com/application/o/gemini/"}) {
		t.Fatalf("additional issuers = %#v", cfg.AdditionalIssuers)
	}
	wantServers := []string{"https://auth.example.com/application/o/chatgpt/", "https://auth.example.com/application/o/gemini/"}
	if !reflect.DeepEqual(cfg.AuthorizationServers, wantServers) {
		t.Fatalf("authorization servers = %#v, want %#v", cfg.AuthorizationServers, wantServers)
	}
}

func TestLoadRejectsDuplicateAdditionalOAuthIssuer(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/chatgpt/")
	t.Setenv("OAUTH_ADDITIONAL_ISSUERS", "https://auth.example.com/application/o/chatgpt/")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "duplicate or primary") {
		t.Fatalf("Load() error = %v, want duplicate issuer rejection", err)
	}
}

func TestLoadRejectsEmptyAdditionalOAuthIssuer(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/chatgpt/")
	t.Setenv("OAUTH_ADDITIONAL_ISSUERS", "https://auth.example.com/application/o/gemini/,,https://auth.example.com/application/o/other/")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must not contain empty values") {
		t.Fatalf("Load() error = %v, want empty issuer rejection", err)
	}
}

func TestLoadRejectsMalformedTypedEnvironment(t *testing.T) {
	t.Setenv("KEEP_SNAPSHOTS", "seven")
	if _, err := Load(); err == nil {
		t.Fatal("expected malformed KEEP_SNAPSHOTS to fail")
	}
}

func TestLoadRelatedFactLowSelectionAndWarnings(t *testing.T) {
	tests := []struct {
		name             string
		related          *string
		legacy           *string
		want             float64
		wantWarning      string
		unwantedLogValue string
	}{
		{name: "default", want: 0.60},
		{name: "canonical only", related: stringPointer("0.62"), want: 0.62},
		{
			name:             "legacy only",
			legacy:           stringPointer("0.61"),
			want:             0.61,
			wantWarning:      "CONTRADICTION_LOW is deprecated; use RELATED_FACT_LOW",
			unwantedLogValue: "0.61",
		},
		{
			name:             "canonical overrides legacy",
			related:          stringPointer("0.63"),
			legacy:           stringPointer("0.64"),
			want:             0.63,
			wantWarning:      "CONTRADICTION_LOW is deprecated and ignored because RELATED_FACT_LOW is set",
			unwantedLogValue: "0.64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSecureTestEnv(t)
			if tt.related != nil {
				t.Setenv("RELATED_FACT_LOW", *tt.related)
			}
			if tt.legacy != nil {
				t.Setenv("CONTRADICTION_LOW", *tt.legacy)
			}

			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previousLogger)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.RelatedFactLow != tt.want {
				t.Fatalf("RelatedFactLow = %v, want %v", cfg.RelatedFactLow, tt.want)
			}

			logOutput := logs.String()
			warningCount := strings.Count(logOutput, "level=WARN")
			if tt.wantWarning == "" {
				if warningCount != 0 {
					t.Fatalf("warning count = %d, want 0; logs: %s", warningCount, logOutput)
				}
				return
			}
			if warningCount != 1 {
				t.Fatalf("warning count = %d, want 1; logs: %s", warningCount, logOutput)
			}
			if !strings.Contains(logOutput, tt.wantWarning) {
				t.Fatalf("logs = %q, want warning %q", logOutput, tt.wantWarning)
			}
			if tt.unwantedLogValue != "" && strings.Contains(logOutput, tt.unwantedLogValue) {
				t.Fatalf("warning leaked configured value %q: %s", tt.unwantedLogValue, logOutput)
			}
		})
	}
}

func TestLoadRelatedFactLowRejectsMalformedActiveSource(t *testing.T) {
	tests := []struct {
		name         string
		related      *string
		legacy       *string
		want         string
		wantWarnings int
	}{
		{
			name:    "canonical only",
			related: stringPointer("not-a-number"),
			want:    "RELATED_FACT_LOW",
		},
		{
			name:    "empty canonical only",
			related: stringPointer(""),
			want:    "RELATED_FACT_LOW",
		},
		{
			name:         "canonical does not fall back to legacy",
			related:      stringPointer("not-a-number"),
			legacy:       stringPointer("0.61"),
			want:         "RELATED_FACT_LOW",
			wantWarnings: 1,
		},
		{
			name:         "empty canonical does not fall back to legacy",
			related:      stringPointer(""),
			legacy:       stringPointer("0.61"),
			want:         "RELATED_FACT_LOW",
			wantWarnings: 1,
		},
		{
			name:         "legacy only",
			legacy:       stringPointer("not-a-number"),
			want:         "CONTRADICTION_LOW",
			wantWarnings: 1,
		},
		{
			name:         "empty legacy only",
			legacy:       stringPointer(""),
			want:         "CONTRADICTION_LOW",
			wantWarnings: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSecureTestEnv(t)
			if tt.related != nil {
				t.Setenv("RELATED_FACT_LOW", *tt.related)
			}
			if tt.legacy != nil {
				t.Setenv("CONTRADICTION_LOW", *tt.legacy)
			}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previousLogger)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want source %q", err, tt.want)
			}
			if got := strings.Count(logs.String(), "level=WARN"); got != tt.wantWarnings {
				t.Fatalf("warning count = %d, want %d; logs: %s", got, tt.wantWarnings, logs.String())
			}
		})
	}
}

func TestLoadRelatedFactLowValidationNamesActiveSource(t *testing.T) {
	tests := []struct {
		name    string
		related *string
		legacy  *string
		dedup   string
		want    string
	}{
		{name: "default", dedup: "0.50", want: "RELATED_FACT_LOW"},
		{name: "canonical range", related: stringPointer("NaN"), dedup: "0.97", want: "RELATED_FACT_LOW"},
		{name: "canonical ordering", related: stringPointer("0.98"), dedup: "0.97", want: "RELATED_FACT_LOW"},
		{name: "legacy range", legacy: stringPointer("+Inf"), dedup: "0.97", want: "CONTRADICTION_LOW"},
		{name: "legacy ordering", legacy: stringPointer("0.98"), dedup: "0.97", want: "CONTRADICTION_LOW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSecureTestEnv(t)
			if tt.related != nil {
				t.Setenv("RELATED_FACT_LOW", *tt.related)
			}
			if tt.legacy != nil {
				t.Setenv("CONTRADICTION_LOW", *tt.legacy)
			}
			t.Setenv("DEDUP_THRESHOLD", tt.dedup)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want source %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsUnsafeRanges(t *testing.T) {
	t.Setenv("API_KEY", "test-key")
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero snapshots", key: "KEEP_SNAPSHOTS", value: "0"},
		{name: "zero chunk size", key: "RAG_CHUNK_MAX_BYTES", value: "0"},
		{name: "zero mutation threshold", key: "MUTATION_MATCH_THRESHOLD", value: "0"},
		{name: "threshold above one", key: "DEDUP_THRESHOLD", value: "1.1"},
		{name: "negative reindex interval", key: "RAG_REINDEX_INTERVAL_MINUTES", value: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s=%q to fail", tt.key, tt.value)
			}
		})
	}
}

func TestLoadTodoistDisabledDoesNotRequireToken(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("ENABLE_TODOIST", "false")
	t.Setenv("TODOIST_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("disabled Todoist must not require TODOIST_TOKEN: %v", err)
	}
	if cfg.EnableTodoist {
		t.Fatal("expected Todoist to remain disabled")
	}
}

func TestLoadTodoistEnabledRequiresTokenAndAPIKey(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		setSecureTestEnv(t)
		t.Setenv("ENABLE_TODOIST", "true")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TODOIST_TOKEN") {
			t.Fatalf("expected TODOIST_TOKEN error, got %v", err)
		}
	})

	t.Run("api key", func(t *testing.T) {
		setSecureTestEnv(t)
		t.Setenv("ENABLE_TODOIST", "true")
		t.Setenv("TODOIST_TOKEN", "todoist-token")
		t.Setenv("API_KEY", "")
		t.Setenv("OAUTH_ENABLED", "true")
		t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/memory")
		t.Setenv("OAUTH_RESOURCE", "https://mcp.example.com")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "API_KEY") {
			t.Fatalf("expected API_KEY error, got %v", err)
		}
	})
}

func TestLoadAuthFailsClosedUnlessExplicitlyAllowed(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("API_KEY", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALLOW_INSECURE_AUTH") {
		t.Fatalf("expected fail-closed auth error, got %v", err)
	}

	t.Setenv("ALLOW_INSECURE_AUTH", "true")
	if _, err := Load(); err != nil {
		t.Fatalf("explicit insecure development mode should load: %v", err)
	}
}

func TestLoadOAuthCanSecureMemoryWithoutAPIKey(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("API_KEY", "")
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com/application/o/memory")
	t.Setenv("OAUTH_RESOURCE", "https://mcp.example.com")
	if _, err := Load(); err != nil {
		t.Fatalf("valid OAuth config should secure memory without an API key: %v", err)
	}
}

func TestLoadOAuthRejectsMissingOrInvalidContract(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("OAUTH_ENABLED", "true")
	t.Setenv("OAUTH_ISSUER", "not-a-url")
	t.Setenv("OAUTH_RESOURCE", "https://mcp.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OAUTH_ISSUER") {
		t.Fatalf("expected issuer URL error, got %v", err)
	}
}

func TestLoadVizRequiresProxySecretOnlyWhenEnabled(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("ENABLE_VIZ", "false")
	t.Setenv("VIZ_PROXY_SECRET", "")
	if _, err := Load(); err != nil {
		t.Fatalf("disabled viz must not require proxy secret: %v", err)
	}

	t.Setenv("ENABLE_VIZ", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIZ_PROXY_SECRET") {
		t.Fatalf("expected VIZ_PROXY_SECRET error, got %v", err)
	}
}

func TestLoadRejectsMalformedBoolean(t *testing.T) {
	t.Setenv("ENABLE_RAG", "yes-please")
	if _, err := Load(); err == nil {
		t.Fatal("expected malformed ENABLE_RAG to fail")
	}
}

func TestLoadIndexerDoesNotRequireOrParseServerFeatures(t *testing.T) {
	t.Setenv("ENABLE_RAG", "true")
	t.Setenv("API_KEY", "")
	t.Setenv("ALLOW_INSECURE_AUTH", "not-a-boolean")
	t.Setenv("ENABLE_TODOIST", "not-a-boolean")
	t.Setenv("TODOIST_TOKEN", "")
	t.Setenv("ENABLE_VIZ", "not-a-boolean")
	t.Setenv("VIZ_PROXY_SECRET", "")
	t.Setenv("OAUTH_ENABLED", "not-a-boolean")

	cfg, err := LoadIndexer()
	if err != nil {
		t.Fatalf("standalone indexer must ignore server-only configuration: %v", err)
	}
	if !cfg.EnableRAG || cfg.QdrantURL == "" || cfg.EmbedURL == "" {
		t.Fatalf("unexpected indexer config: %#v", cfg)
	}
}

func TestLoadIndexerStrictlyValidatesIndexerSettings(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "RAG disabled", key: "ENABLE_RAG", value: "false", want: "ENABLE_RAG"},
		{name: "invalid Qdrant URL", key: "QDRANT_URL", value: "not-a-url", want: "QDRANT_URL"},
		{name: "invalid embeddings URL", key: "EMBED_URL", value: "ftp://example.com", want: "EMBED_URL"},
		{name: "invalid chunk size", key: "RAG_CHUNK_MAX_BYTES", value: "0", want: "RAG_CHUNK_MAX_BYTES"},
		{name: "invalid folder threshold", key: "RAG_FOLDER_THRESHOLD", value: "1.1", want: "RAG_FOLDER_THRESHOLD"},
		{name: "malformed interval", key: "RAG_REINDEX_INTERVAL_MINUTES", value: "soon", want: "RAG_REINDEX_INTERVAL_MINUTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENABLE_RAG", "true")
			t.Setenv(tt.key, tt.value)
			_, err := LoadIndexer()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadIndexer error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadEmbeddingIdentityDefaults(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("EMBED_MODEL", "")
	t.Setenv("EMBED_MODEL_REVISION", "")
	t.Setenv("EMBED_INPUT_PROFILE", "")
	t.Setenv("ADOPT_EXISTING_EMBEDDING_IDENTITY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.EmbedModelID != defaultEmbedModelID {
		t.Fatalf("EmbedModelID = %q, want %q", cfg.EmbedModelID, defaultEmbedModelID)
	}
	if cfg.EmbedModelRevision != defaultEmbedModelRevision {
		t.Fatalf("EmbedModelRevision = %q, want %q", cfg.EmbedModelRevision, defaultEmbedModelRevision)
	}
	if cfg.EmbedInputProfile != embeddings.LegacyRawV1 {
		t.Fatalf("EmbedInputProfile = %q, want %q", cfg.EmbedInputProfile, embeddings.LegacyRawV1)
	}
	if cfg.AdoptExistingEmbeddingIdentity {
		t.Fatal("AdoptExistingEmbeddingIdentity must default to false")
	}
}

func TestLoadEmbeddingInputProfile(t *testing.T) {
	t.Run("non-legacy profile is eval-only", func(t *testing.T) {
		setSecureTestEnv(t)
		t.Setenv("EMBED_INPUT_PROFILE", string(embeddings.MultilingualE5V1))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "production runtime supports only") {
			t.Fatalf("Load() error = %v, want production profile rejection", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		setSecureTestEnv(t)
		t.Setenv("EMBED_INPUT_PROFILE", "future-v9")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EMBED_INPUT_PROFILE") {
			t.Fatalf("Load() error = %v, want EMBED_INPUT_PROFILE validation error", err)
		}
	})

	t.Run("unsupported model", func(t *testing.T) {
		setSecureTestEnv(t)
		t.Setenv("EMBED_MODEL", "example/other-model")
		t.Setenv("EMBED_INPUT_PROFILE", string(embeddings.MultilingualE5V1))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EMBED_INPUT_PROFILE") {
			t.Fatalf("Load() error = %v, want model/profile validation error", err)
		}
	})
}

func TestLoadRejectsMutableOrMalformedEmbeddingRevision(t *testing.T) {
	tests := []string{"main", "latest", "614241f", strings.Repeat("z", 40), strings.Repeat("a", 39), strings.Repeat("a", 41)}
	for _, revision := range tests {
		t.Run(revision, func(t *testing.T) {
			setSecureTestEnv(t)
			t.Setenv("EMBED_MODEL_REVISION", revision)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "EMBED_MODEL_REVISION") {
				t.Fatalf("Load() error = %v, want EMBED_MODEL_REVISION validation error", err)
			}
		})
	}
}

func TestLoadEmbeddingIdentityAdoptionFlag(t *testing.T) {
	setSecureTestEnv(t)
	t.Setenv("ADOPT_EXISTING_EMBEDDING_IDENTITY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.AdoptExistingEmbeddingIdentity {
		t.Fatal("AdoptExistingEmbeddingIdentity = false, want true")
	}

	t.Setenv("ADOPT_EXISTING_EMBEDDING_IDENTITY", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADOPT_EXISTING_EMBEDDING_IDENTITY") {
		t.Fatalf("Load() error = %v, want adoption flag validation error", err)
	}
}

func TestLoadIndexerIncludesEmbeddingIdentityContract(t *testing.T) {
	t.Setenv("ENABLE_RAG", "true")
	t.Setenv("EMBED_MODEL", "example/model")
	t.Setenv("EMBED_MODEL_REVISION", strings.Repeat("b", 40))
	t.Setenv("EMBED_INPUT_PROFILE", string(embeddings.LegacyRawV1))
	t.Setenv("ADOPT_EXISTING_EMBEDDING_IDENTITY", "true")

	cfg, err := LoadIndexer()
	if err != nil {
		t.Fatalf("LoadIndexer() error = %v", err)
	}
	if cfg.EmbedModelID != "example/model" || cfg.EmbedModelRevision != strings.Repeat("b", 40) {
		t.Fatalf("unexpected embedding identity config: %#v", cfg)
	}
	if cfg.EmbedInputProfile != embeddings.LegacyRawV1 {
		t.Fatalf("EmbedInputProfile = %q", cfg.EmbedInputProfile)
	}
	if !cfg.AdoptExistingEmbeddingIdentity {
		t.Fatal("standalone indexer did not load adoption flag")
	}

	t.Setenv("EMBED_INPUT_PROFILE", "future-v9")
	if _, err := LoadIndexer(); err == nil || !strings.Contains(err.Error(), "EMBED_INPUT_PROFILE") {
		t.Fatalf("LoadIndexer() error = %v, want EMBED_INPUT_PROFILE validation error", err)
	}

	t.Setenv("EMBED_MODEL", defaultEmbedModelID)
	t.Setenv("EMBED_INPUT_PROFILE", string(embeddings.MultilingualE5V1))
	if _, err := LoadIndexer(); err == nil || !strings.Contains(err.Error(), "production runtime supports only") {
		t.Fatalf("LoadIndexer() error = %v, want production profile rejection", err)
	}
}

func TestEnvCSVEmpty(t *testing.T) {
	key := "TEST_EMPTY_CSV"
	_ = os.Unsetenv(key)
	if got := envCSV(key); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
}

func setSecureTestEnv(t *testing.T) {
	t.Helper()
	unsetTestEnv(t, "RELATED_FACT_LOW")
	unsetTestEnv(t, "CONTRADICTION_LOW")
	t.Setenv("API_KEY", "test-key")
	t.Setenv("ALLOW_INSECURE_AUTH", "false")
	t.Setenv("ENABLE_TODOIST", "false")
	t.Setenv("TODOIST_TOKEN", "")
	t.Setenv("ENABLE_VIZ", "false")
	t.Setenv("VIZ_PROXY_SECRET", "")
	t.Setenv("OAUTH_ENABLED", "false")
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("OAUTH_AUDIENCE", "")
	t.Setenv("OAUTH_JWKS_URL", "")
	t.Setenv("OAUTH_AUTHORIZATION_SERVERS", "")
	t.Setenv("EMBED_MODEL", defaultEmbedModelID)
	t.Setenv("EMBED_MODEL_REVISION", defaultEmbedModelRevision)
	t.Setenv("EMBED_INPUT_PROFILE", string(embeddings.LegacyRawV1))
	t.Setenv("ADOPT_EXISTING_EMBEDDING_IDENTITY", "false")
}

func stringPointer(value string) *string {
	return &value
}

func unsetTestEnv(t *testing.T, key string) {
	t.Helper()
	value, set := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if set {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("restore unset %s: %v", key, err)
		}
	})
}
