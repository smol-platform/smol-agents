package mcp

// memoryTools is the static list advertised by tools/list.
// Each entry matches one tool defined by R-MEM-MCP-1.
var memoryTools = []Tool{
	{
		Name:        "retrieve_memory",
		Description: "Search memory for relevant documents. Returns the top-K ranked results within the caller's tenant.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "Natural-language search query"},
				"topK":         map[string]any{"type": "integer", "description": "Maximum results (clamped to quota ceiling)"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"namespace":    map[string]any{"type": "string", "description": "Memory namespace to search"},
				"filters":      map[string]any{"type": "object", "description": "Optional metadata key/value predicates"},
			},
			"required": []string{"query", "retrieverRef"},
		},
	},
	{
		Name:        "write_memory",
		Description: "Store a document in memory within the caller's tenant and namespace. When the retriever requires mutation authorization (MutationsTraT=true), a Transaction Token must be supplied in the 'trat' field.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":      map[string]any{"type": "string", "description": "Document content to store"},
				"namespace":    map[string]any{"type": "string", "description": "Target memory namespace"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"metadata":     map[string]any{"type": "object", "description": "Optional key/value metadata"},
				"id":           map[string]any{"type": "string", "description": "Optional stable document ID (upsert if exists)"},
				"trat":         map[string]any{"type": "string", "description": "Transaction Token (TraT) — required when the retriever has MutationsTraT=true. Obtain from the agentnet sidecar token-exchange endpoint."},
			},
			"required": []string{"content", "retrieverRef"},
		},
	},
	{
		Name:        "list_memory_namespaces",
		Description: "List the memory namespaces accessible to the caller within the given retriever.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
			},
			"required": []string{"retrieverRef"},
		},
	},
	{
		Name:        "get_memory",
		Description: "Fetch a single document by ID. The document must belong to the caller's tenant.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":           map[string]any{"type": "string", "description": "Stable document identifier"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"namespace":    map[string]any{"type": "string", "description": "Memory namespace containing the document"},
			},
			"required": []string{"id", "retrieverRef"},
		},
	},
	{
		Name:        "delete_memory",
		Description: "Delete a document by ID. The document must belong to the caller's tenant. When the retriever requires mutation authorization (MutationsTraT=true), a Transaction Token must be supplied in the 'trat' field.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":           map[string]any{"type": "string", "description": "Stable document identifier"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"namespace":    map[string]any{"type": "string", "description": "Memory namespace containing the document"},
				"trat":         map[string]any{"type": "string", "description": "Transaction Token (TraT) — required when the retriever has MutationsTraT=true. Obtain from the agentnet sidecar token-exchange endpoint."},
			},
			"required": []string{"id", "retrieverRef"},
		},
	},
	{
		Name:        "summarize_memory",
		Description: "Generate a natural-language summary of documents matching the query within the caller's tenant. (P2: may return not-supported.)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "Topic to summarize"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"namespace":    map[string]any{"type": "string", "description": "Memory namespace to summarize"},
			},
			"required": []string{"query", "retrieverRef"},
		},
	},
	{
		Name:        "merge_memory_fs",
		Description: "Fast-forward publish srcBranch into dstBranch in a filesystem-backed memory store. All files from srcBranch are applied onto dstBranch (CoW semantics). Requires write permission on dstBranch's namespace. Only supported for kind=filesystem backends.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"srcBranch":    map[string]any{"type": "string", "description": "Branch whose files are applied onto dstBranch"},
				"dstBranch":    map[string]any{"type": "string", "description": "Branch that receives the merged files"},
				"retrieverRef": map[string]any{"type": "string", "description": "Namespace-qualified MemoryRetriever name (ns/name)"},
				"namespace":    map[string]any{"type": "string", "description": "Memory namespace containing both branches"},
			},
			"required": []string{"srcBranch", "dstBranch", "retrieverRef"},
		},
	},
}

// memoryResources is the static list advertised by resources/list.
// Each entry matches one resource pattern defined by R-MEM-MCP-2.
var memoryResources = []Resource{
	{
		URI:         "memory://namespaces/{namespace}",
		Name:        "Memory namespace",
		Description: "Lists documents in a specific memory namespace within the caller's tenant.",
		MIMEType:    "application/json",
	},
	{
		URI:         "memory://retrievers/{retrieverRef}",
		Name:        "Memory retriever",
		Description: "Returns configuration and status of a MemoryRetriever visible to the caller.",
		MIMEType:    "application/json",
	},
	{
		URI:         "memory://documents/{id}",
		Name:        "Memory document",
		Description: "Fetches a single document by ID, scoped to the caller's tenant.",
		MIMEType:    "application/json",
	},
	{
		URI:         "memory://episodes/{agentId}",
		Name:        "Agent episodes",
		Description: "Lists episode records for an agent run, scoped to the caller's tenant.",
		MIMEType:    "application/json",
	},
}
