const dbName = "ai_gateway";
const dbRef = db.getSiblingDB(dbName);

// 1. Create semantic_cache collection and standard indexes
dbRef.createCollection("semantic_cache");

dbRef.semantic_cache.createIndex(
    { expires_at: 1 },
    { expireAfterSeconds: 0, name: "ttl_expires_at" }
);

dbRef.semantic_cache.createIndex(
    { tenant_id: 1, model: 1, prompt_hash: 1 },
    { unique: true, name: "uq_tenant_model_hash" }
);

// 2. Create Atlas Vector Search index (768 dimensions for text-embedding-004)
try {
    dbRef.semantic_cache.createSearchIndex(
        "vector_idx",
        "vectorSearch",
        {
            fields: [
                {
                    type: "vector",
                    path: "embedding",
                    numDimensions: 768,
                    similarity: "cosine"
                },
                {
                    type: "filter",
                    path: "tenant_id"
                },
                {
                    type: "filter",
                    path: "model"
                }
            ]
        }
    );
    print("Created vector_idx on semantic_cache");
} catch (e) {
    print("Vector search index creation note: " + e);
}

// 3. Create time-series collection for usage_telemetry
if (!dbRef.getCollectionNames().includes("usage_telemetry")) {
    dbRef.createCollection("usage_telemetry", {
        timeseries: {
            timeField: "timestamp",
            metaField: "meta",
            granularity: "minutes"
        },
        expireAfterSeconds: 2592000 // 30-day retention
    });
    print("Created usage_telemetry time-series collection");
}