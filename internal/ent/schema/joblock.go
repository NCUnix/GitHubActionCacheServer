package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

type JobLock struct {
	ent.Schema
}

func (JobLock) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "job_locks"},
	}
}

func (JobLock) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			MaxLen(64).
			NotEmpty().
			Immutable().
			SchemaType(originalBoundedStringColumnType(64)),
	}
}
