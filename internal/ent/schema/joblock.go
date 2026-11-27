package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// JobLock rows coordinate jobs across replicas: the row lock serializes mutually
// exclusive jobs, and lastStartedAt limits once-per-interval jobs to one start.
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
		field.Int64("lastStartedAt").Optional().Nillable().StorageKey("lastStartedAt"),
	}
}
