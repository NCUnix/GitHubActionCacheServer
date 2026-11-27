package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type StorageManifest struct{ ent.Schema }

func (StorageManifest) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "storage_manifests"}}
}

func (StorageManifest) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("uploadId").Optional().Nillable().StorageKey("uploadId"),
		field.String("locationId").Optional().Nillable().StorageKey("locationId").SchemaType(originalIDColumnType),
		field.Bytes("parts").SchemaType(map[string]string{dialect.MySQL: "mediumblob"}),
		field.Int64("candidateGCAt").Optional().Nillable().StorageKey("candidateGCAt"),
	}
}

func (StorageManifest) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("upload", Upload.Type).Ref("manifest").Field("uploadId").Unique(),
		edge.From("location", StorageLocation.Type).Ref("manifest").Field("locationId").Unique(),
	}
}

func (StorageManifest) Indexes() []ent.Index {
	return []ent.Index{index.Fields("candidateGCAt", "id")}
}
