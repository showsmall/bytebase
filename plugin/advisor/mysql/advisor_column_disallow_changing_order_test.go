package mysql

// Framework code is generated by the generator.

import (
	"testing"

	"github.com/bytebase/bytebase/plugin/advisor"
)

func TestColumnDisallowChangingOrder(t *testing.T) {
	tests := []advisor.TestCase{
		{
			Statement: `
				CREATE TABLE t(a int);
				ALTER TABLE t MODIFY COLUMN a int`,
			Want: []advisor.Advice{
				{
					Status:  advisor.Success,
					Code:    advisor.Ok,
					Title:   "OK",
					Content: "",
				},
			},
		},
		{
			Statement: `
				CREATE TABLE t(a int);
				ALTER TABLE t MODIFY COLUMN a int FIRST`,
			Want: []advisor.Advice{
				{
					Status:  advisor.Warn,
					Code:    advisor.ChangeColumnOrder,
					Title:   "column.disallow-changing-order",
					Content: "\"ALTER TABLE t MODIFY COLUMN a int FIRST\" changes column order",
					Line:    3,
				},
			},
		},
		{
			Statement: `
				CREATE TABLE t(b int, a1 int);
				ALTER TABLE t CHANGE COLUMN a1 a int FIRST`,
			Want: []advisor.Advice{
				{
					Status:  advisor.Warn,
					Code:    advisor.ChangeColumnOrder,
					Title:   "column.disallow-changing-order",
					Content: "\"ALTER TABLE t CHANGE COLUMN a1 a int FIRST\" changes column order",
					Line:    3,
				},
			},
		},
		{
			Statement: `
				CREATE TABLE t(a int, b int);
				ALTER TABLE t MODIFY COLUMN a int AFTER b`,
			Want: []advisor.Advice{
				{
					Status:  advisor.Warn,
					Code:    advisor.ChangeColumnOrder,
					Title:   "column.disallow-changing-order",
					Content: "\"ALTER TABLE t MODIFY COLUMN a int AFTER b\" changes column order",
					Line:    3,
				},
			},
		},
		{
			Statement: `
				CREATE TABLE t(a1 int, b int);
				ALTER TABLE t CHANGE COLUMN a1 a int AFTER b`,
			Want: []advisor.Advice{
				{
					Status:  advisor.Warn,
					Code:    advisor.ChangeColumnOrder,
					Title:   "column.disallow-changing-order",
					Content: "\"ALTER TABLE t CHANGE COLUMN a1 a int AFTER b\" changes column order",
					Line:    3,
				},
			},
		},
	}

	advisor.RunSQLReviewRuleTests(t, tests, &ColumnDisallowChangingOrderAdvisor{}, &advisor.SQLReviewRule{
		Type:    advisor.SchemaRuleColumnDisallowChangingOrder,
		Level:   advisor.SchemaRuleLevelWarning,
		Payload: "",
	}, advisor.MockMySQLDatabase)
}
