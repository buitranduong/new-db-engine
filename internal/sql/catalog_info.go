package sql

import (
	"stask/internal/schema"
)

// The information_schema views (https://www.postgresql.org/docs/16/information-schema.html)
// over the same schema as pg_catalog. Their names carry the schema
// ("information_schema.tables"): unlike pg_catalog, information_schema is
// not on the search path.

const infoPrefix = "information_schema."

func defInfo(name, cols string, rows func(c *catSnap) [][]Datum) { defCat(infoPrefix+name, cols, rows) }

// colTypeInfo is a column's information_schema data_type and udt_name.
func colTypeInfo(c schema.Column) (dataType, udt string, ty *pgType) {
	ty = pgTypeByOID[colTypeOID(c)]
	return ty.infoName, ty.name, ty
}

// numericInfo is numeric_precision, numeric_precision_radix, numeric_scale
// and datetime_precision of a type.
func numericInfo(oid int64) (prec, radix, scale, dt Datum) {
	prec, radix, scale, dt = Null, Null, Null, Null
	switch oid {
	case 20:
		prec, radix, scale = IntD(64), IntD(2), IntD(0)
	case 23:
		prec, radix, scale = IntD(32), IntD(2), IntD(0)
	case 21:
		prec, radix, scale = IntD(16), IntD(2), IntD(0)
	case 701:
		prec, radix = IntD(53), IntD(2)
	case 1184, 1114:
		dt = IntD(3) // milliseconds
	case 1082:
		dt = IntD(0)
	}
	return
}

func init() {
	cat := TextD("stask")
	pub := TextD("public")
	defInfo("schemata", "catalog_name:t schema_name:t schema_owner:t default_character_set_catalog:t "+
		"default_character_set_schema:t default_character_set_name:t sql_path:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, n := range []string{"information_schema", "pg_catalog", "public"} {
			rows = append(rows, []Datum{cat, TextD(n), TextD(roleName), Null, Null, Null, Null})
		}
		return rows
	})

	defInfo("tables", "table_catalog:t table_schema:t table_name:t table_type:t self_referencing_column_name:t "+
		"reference_generation:t user_defined_type_catalog:t user_defined_type_schema:t user_defined_type_name:t "+
		"is_insertable_into:t is_typed:t commit_action:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			rows = append(rows, []Datum{cat, pub, TextD(t.Name), TextD("BASE TABLE"), Null, Null, Null, Null, Null,
				TextD("YES"), TextD("NO"), Null})
		}
		return rows
	})

	defInfo("columns", "table_catalog:t table_schema:t table_name:t column_name:t ordinal_position:i "+
		"column_default:t is_nullable:t data_type:t character_maximum_length:i character_octet_length:i "+
		"numeric_precision:i numeric_precision_radix:i numeric_scale:i datetime_precision:i interval_type:t "+
		"interval_precision:i character_set_catalog:t character_set_schema:t character_set_name:t "+
		"collation_catalog:t collation_schema:t collation_name:t domain_catalog:t domain_schema:t domain_name:t "+
		"udt_catalog:t udt_schema:t udt_name:t scope_catalog:t scope_schema:t scope_name:t maximum_cardinality:i "+
		"dtd_identifier:t is_self_referencing:t is_identity:t identity_generation:t identity_start:t "+
		"identity_increment:t identity_maximum:t identity_minimum:t identity_cycle:t is_generated:t "+
		"generation_expression:t is_updatable:t", func(c *catSnap) [][]Datum {
		seqs := map[string]int{}
		for i, q := range c.seqs {
			seqs[q.Name] = i
		}
		var rows [][]Datum
		for _, t := range c.tables {
			for _, col := range t.Columns {
				dataType, udt, ty := colTypeInfo(col)
				prec, radix, scale, dt := numericInfo(ty.oid)
				octet := Null
				if ty.cat == "S" {
					octet = IntD(1073741824)
				}
				def := Null
				if hasDef(col) {
					def = TextD(defaultText(col))
				}
				isID, gen := "NO", Null
				idStart, idInc, idMax, idMin, idCycle := Null, Null, Null, Null, Null
				if col.Identity != "" {
					isID, gen = "YES", TextD(map[string]string{"always": "ALWAYS", "by default": "BY DEFAULT"}[col.Identity])
					if i, ok := seqs[t.Name+"_"+col.Name+"_seq"]; ok {
						q := c.seqs[i]
						idStart, idInc, idMax, idMin = TextD(itoa(q.Start)), TextD(itoa(q.Inc)), TextD(itoa(q.Max)), TextD(itoa(q.Min))
						idCycle = TextD(boolStr(q.Cycle))
					}
				}
				rows = append(rows, []Datum{cat, pub, TextD(t.Name), TextD(col.Name), IntD(int64(col.Num)), def,
					TextD(boolStr(!(col.NotNull || col.Name == t.Key))), TextD(dataType), Null, octet, prec, radix, scale, dt,
					Null, Null, Null, Null, Null, Null, Null, Null, Null, Null, Null, cat, TextD("pg_catalog"), TextD(udt),
					Null, Null, Null, Null, TextD(itoa(int64(col.Num))), TextD("NO"), TextD(isID), gen, idStart, idInc,
					idMax, idMin, idCycle, TextD("NEVER"), Null, TextD("YES")})
			}
		}
		return rows
	})

	defInfo("sequences", "sequence_catalog:t sequence_schema:t sequence_name:t data_type:t numeric_precision:i "+
		"numeric_precision_radix:i numeric_scale:i start_value:t minimum_value:t maximum_value:t increment:t "+
		"cycle_option:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, q := range c.seqs {
			prec, radix, scale, _ := numericInfo(seqTypeOID(q.Type))
			rows = append(rows, []Datum{cat, pub, TextD(q.Name), TextD(q.Type), prec, radix, scale, TextD(itoa(q.Start)),
				TextD(itoa(q.Min)), TextD(itoa(q.Max)), TextD(itoa(q.Inc)), TextD(boolStr(q.Cycle))})
		}
		return rows
	})

	defInfo("table_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t table_catalog:t "+
		"table_schema:t table_name:t constraint_type:t is_deferrable:t initially_deferred:t enforced:t "+
		"nulls_distinct:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		con := func(name string, t *schema.Table, typ string, nd Datum) {
			rows = append(rows, []Datum{cat, pub, TextD(name), cat, pub, TextD(t.Name), TextD(typ), TextD("NO"),
				TextD("NO"), TextD("YES"), nd})
		}
		for _, t := range c.tables {
			con(pkeyName(t), t, "PRIMARY KEY", TextD("YES"))
			for _, ck := range t.Checks {
				con(ck.Name, t, "CHECK", Null)
			}
		}
		return rows
	})

	defInfo("check_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t check_clause:t",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, t := range c.tables {
				for _, ck := range t.Checks {
					rows = append(rows, []Datum{cat, pub, TextD(ck.Name), TextD(ck.Expr)})
				}
			}
			return rows
		})

	defInfo("key_column_usage", "constraint_catalog:t constraint_schema:t constraint_name:t table_catalog:t "+
		"table_schema:t table_name:t column_name:t ordinal_position:i position_in_unique_constraint:i",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, t := range c.tables {
				rows = append(rows, []Datum{cat, pub, TextD(pkeyName(t)), cat, pub, TextD(t.Name), TextD(t.Key), IntD(1), Null})
			}
			return rows
		})

	defInfo("constraint_column_usage", "table_catalog:t table_schema:t table_name:t column_name:t "+
		"constraint_catalog:t constraint_schema:t constraint_name:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			rows = append(rows, []Datum{cat, pub, TextD(t.Name), TextD(t.Key), cat, pub, TextD(pkeyName(t))})
			for _, ck := range t.Checks {
				for _, n := range parseArrayText(checkKeys(t, ck.Expr)) {
					for _, col := range t.Columns {
						if int64(col.Num) == n.I {
							rows = append(rows, []Datum{cat, pub, TextD(t.Name), TextD(col.Name), cat, pub, TextD(ck.Name)})
						}
					}
				}
			}
		}
		return rows
	})

	// every table is owned by the one role, which may do everything
	defInfo("table_privileges", "grantor:t grantee:t table_catalog:t table_schema:t table_name:t privilege_type:t "+
		"is_grantable:t with_hierarchy:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			for _, p := range []string{"INSERT", "SELECT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				rows = append(rows, []Datum{TextD(roleName), TextD(roleName), cat, pub, TextD(t.Name), TextD(p), TextD("YES"), TextD("YES")})
			}
		}
		return rows
	})
	defInfo("routines", "specific_catalog:t specific_schema:t specific_name:t routine_catalog:t routine_schema:t "+
		"routine_name:t routine_type:t data_type:t routine_definition:t external_language:t", nil)
	defInfo("triggers", "trigger_catalog:t trigger_schema:t trigger_name:t event_manipulation:t "+
		"event_object_catalog:t event_object_schema:t event_object_table:t action_order:i action_condition:t "+
		"action_statement:t action_orientation:t action_timing:t", nil)
	defInfo("domains", "domain_catalog:t domain_schema:t domain_name:t data_type:t", nil)
	defInfo("parameters", "specific_catalog:t specific_schema:t specific_name:t ordinal_position:i parameter_mode:t "+
		"parameter_name:t data_type:t", nil)
	defInfo("referential_constraints", "constraint_catalog:t constraint_schema:t constraint_name:t "+
		"unique_constraint_catalog:t unique_constraint_schema:t unique_constraint_name:t match_option:t "+
		"update_rule:t delete_rule:t", nil)
	defInfo("views", "table_catalog:t table_schema:t table_name:t view_definition:t check_option:t "+
		"is_updatable:t is_insertable_into:t is_trigger_updatable:t is_trigger_deletable:t "+
		"is_trigger_insertable_into:t", nil)
}
