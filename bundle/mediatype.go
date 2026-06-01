package bundle

// OCI layer media type constants for configbundle artifacts.
// Import this package everywhere — do not hardcode these strings.
const (
	// MediaTypeManifest is the ConfigBundle manifest layer produced by the bundler enricher.
	MediaTypeManifest = "application/vnd.armada.configbundle.manifest.v1+yaml"

	// MediaTypeData is the DGraph subgraph data layer (data.json.gz) produced by Orbital.
	MediaTypeData = "application/vnd.orbital.subgraph.data.v1+gzip"

	// MediaTypeSchema is the DGraph schema layer (schema.gz) produced by Orbital.
	MediaTypeSchema = "application/vnd.orbital.subgraph.schema.v1+gzip"
)
