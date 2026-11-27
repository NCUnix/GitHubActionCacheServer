package ent

import (
	"context"

	"entgo.io/ent/dialect/sql"
)

func (c *Client) Dialect() string {
	return c.driver.Dialect()
}

func (c *Client) QuerySelector(ctx context.Context, build func(*sql.DialectBuilder) *sql.Selector) (*sql.Rows, error) {
	selector := build(sql.Dialect(c.Dialect()))
	query, args := selector.Query()
	rows := &sql.Rows{}
	if err := c.driver.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	return rows, nil
}
