package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UploadBlock struct{ ent.Schema }

func (UploadBlock) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "upload_blocks"}}
}

func (UploadBlock) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("uploadId").Immutable().StorageKey("uploadId"),
		field.String("blockHash").Immutable().StorageKey("blockHash").SchemaType(originalBoundedStringColumnType(64)),
		field.String("candidateId").StorageKey("candidateId").SchemaType(originalIDColumnType),
		field.Int64("sizeBytes").NonNegative().StorageKey("sizeBytes"),
	}
}

func (UploadBlock) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("upload", Upload.Type).Ref("blocks").Field("uploadId").Unique().Required().Immutable(),
	}
}

func (UploadBlock) Indexes() []ent.Index {
	return []ent.Index{index.Fields("uploadId", "blockHash").Unique()}
}
