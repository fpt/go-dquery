// Package fixture provides the shared test schema: users -> orders -> items.
package fixture

import (
	"time"

	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/value"
)

const ShopYAML = `
relations:
  - name: users
    columns:
      - {name: id, type: int}
      - {name: name, type: string}
      - {name: email, type: string, nullable: true}
    primary: {partition: [id]}
    paths:
      - {name: by_email, partition: [email], unique: true}

  - name: orders
    columns:
      - {name: id, type: int}
      - {name: user_id, type: int}
      - {name: status, type: string}
      - {name: amount, type: float}
      - {name: created_at, type: timestamp}
    primary: {partition: [id]}
    paths:
      - {name: by_user, partition: [user_id], sort: [created_at]}
      - {name: by_status, partition: [status], sort: [created_at]}

  - name: items
    columns:
      - {name: order_id, type: int}
      - {name: line, type: int}
      - {name: product, type: string}
      - {name: qty, type: int}
    primary: {partition: [order_id], sort: [line]}

relationships:
  - {name: orders, from: users, from_cols: [id], to: orders, path: by_user, kind: one_to_many}
  - {name: user, from: orders, from_cols: [user_id], to: users, kind: many_to_one}
  - {name: items, from: orders, from_cols: [id], to: items, kind: one_to_many}
`

// Shop returns the shop catalog.
func Shop() *schema.Catalog {
	c, err := schema.Parse([]byte(ShopYAML))
	if err != nil {
		panic(err)
	}
	return c
}

// Epoch is the base time used for created_at in fixtures.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// At returns Epoch plus d days.
func At(days int) value.Value { return value.Timestamp(Epoch.AddDate(0, 0, days)) }

func User(id int64, name string, email any) value.Row {
	e := value.Null
	if s, ok := email.(string); ok {
		e = value.String(s)
	}
	return value.Row{value.Int(id), value.String(name), e}
}

func Order(id, userID int64, status string, amount float64, day int) value.Row {
	return value.Row{value.Int(id), value.Int(userID), value.String(status), value.Float(amount), At(day)}
}

func Item(orderID, line int64, product string, qty int64) value.Row {
	return value.Row{value.Int(orderID), value.Int(line), value.String(product), value.Int(qty)}
}
