package sql

import "strings"

// unaccent(text) of the unaccent extension: letters with diacritics become
// their base letters. The table covers Latin-1, Latin Extended-A and
// Vietnamese (PostgreSQL's unaccent.rules maps the same characters).

var unaccentMap = func() map[rune]string {
	m := map[rune]string{}
	add := func(base string, chars string) {
		for _, r := range chars {
			m[r] = base
		}
	}
	add("a", "àáâãäåāăąǎǟǡǻȁȃȧạảấầẩẫậắằẳẵặ")
	add("A", "ÀÁÂÃÄÅĀĂĄǍǞǠǺȀȂȦẠẢẤẦẨẪẬẮẰẲẴẶ")
	add("c", "çćĉċč")
	add("C", "ÇĆĈĊČ")
	add("d", "ďđ")
	add("D", "ĎĐ")
	add("e", "èéêëēĕėęěȅȇȩẹẻẽếềểễệ")
	add("E", "ÈÉÊËĒĔĖĘĚȄȆȨẸẺẼẾỀỂỄỆ")
	add("g", "ĝğġģǧǵ")
	add("G", "ĜĞĠĢǦǴ")
	add("h", "ĥħ")
	add("H", "ĤĦ")
	add("i", "ìíîïĩīĭįǐȉȋỉị")
	add("I", "ÌÍÎÏĨĪĬĮİǏȈȊỈỊ")
	add("j", "ĵ")
	add("J", "Ĵ")
	add("k", "ķ")
	add("K", "Ķ")
	add("l", "ĺļľŀł")
	add("L", "ĹĻĽĿŁ")
	add("n", "ñńņňǹ")
	add("N", "ÑŃŅŇǸ")
	add("o", "òóôõöøōŏőơǒǫǭȍȏȫȭȯȱọỏốồổỗộớờởỡợ")
	add("O", "ÒÓÔÕÖØŌŎŐƠǑǪǬȌȎȪȬȮȰỌỎỐỒỔỖỘỚỜỞỠỢ")
	add("r", "ŕŗřȑȓ")
	add("R", "ŔŖŘȐȒ")
	add("s", "śŝşšș")
	add("S", "ŚŜŞŠȘ")
	add("t", "ţťŧț")
	add("T", "ŢŤŦȚ")
	add("u", "ùúûüũūŭůűųưǔǖǘǚǜȕȗụủứừửữự")
	add("U", "ÙÚÛÜŨŪŬŮŰŲƯǓǕǗǙǛȔȖỤỦỨỪỬỮỰ")
	add("w", "ŵ")
	add("W", "Ŵ")
	add("y", "ýÿŷỳỵỷỹ")
	add("Y", "ÝŶŸỲỴỶỸ")
	add("z", "źżž")
	add("Z", "ŹŻŽ")
	m['æ'], m['Æ'], m['œ'], m['Œ'], m['ß'] = "ae", "AE", "oe", "OE", "ss"
	// combining marks left from decomposed input
	for r := rune(0x300); r <= 0x36f; r++ {
		m[r] = ""
	}
	return m
}()

func unaccent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if v, ok := unaccentMap[r]; ok {
			b.WriteString(v)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
