package sql

import "stask/internal/schema"

// srcOf is the table OID and attribute number RowDescription reports for
// column i of t. Task jmixfix.
func srcOf(t *schema.Table, i int) (uint32, int16) {
	if t.Columns[i].Hidden {
		return 0, 0
	}
	n := t.Columns[i].Num
	if n == 0 {
		n = i + 1
	}
	num := n
	for _, cs := range [][]schema.Column{t.Columns, t.Dropped} { // clients see the numbering without hidden columns (ClientView)
		for _, col := range cs {
			if col.Hidden && col.Num < num {
				n--
			}
		}
	}
	return uint32(tableOID(t)), int16(n)
}

// withSrc copies the table source of a result column.
func (c Column) withSrc(r relCol) Column {
	c.TableOID, c.AttNum = r.src, r.an
	return c
}

// withTable sets the table source of a column of table t, by name.
func (c Column) withTable(t *schema.Table) Column {
	if i, ok := t.Col(c.Name); ok {
		c.TableOID, c.AttNum = srcOf(t, i)
	}
	return c
}

// withItemSrc sets it for a select item that is a plain column of the
// single table of sc.
func (c Column) withItemSrc(it SelItem, sc *scope) Column {
	if e, ok := it.X.(*ColRef); ok && sc.t != nil {
		if i, err := sc.resolve(e); err == nil {
			c.TableOID, c.AttNum = srcOf(sc.t, i)
		}
	}
	return c
}
