// collfid measures how well STASK text collation orders match PostgreSQL's
// (glibc en_US.utf8) ORDER BY. Corpora: Vietnamese names, CRM REST values,
// ASCII words. Needs a postgres container (docker exec) named by -container.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"

	"stask/internal/sql"
)

var (
	given  = strings.Fields("An Anh Ánh Bình Bảo Châu Chi Cường Dũng Dương Duy Đạt Đức Giang Hà Hải Hạnh Hiền Hiếu Hoa Hoàng Hồng Huy Hùng Hương Khánh Khoa Kiên Lan Linh Long Lộc Mai Minh My Nam Nga Ngân Ngọc Nhân Nhi Nhung Phát Phúc Phương Quân Quang Quyên Sơn Tâm Thảo Thành Thắng Thịnh Thu Thủy Thúy Tiến Trang Trí Trung Tú Tuấn Tùng Uyên Vân Việt Vinh Vũ Vy Xuân Yến Ân Ơn Ý Ðạt Ðông")
	middle = strings.Fields("Văn Thị Hữu Đình Ngọc Quốc Minh Thanh Xuân Kim Hoàng Bảo Gia Anh Đức Thế Trọng Công Phương Mai Thu Hồng Tuấn Ðăng")
	family = strings.Fields("Nguyễn Trần Lê Phạm Hoàng Huỳnh Phan Vũ Võ Đặng Bùi Đỗ Hồ Ngô Dương Lý Đào Đinh Lương Trịnh Tạ Ông Ưng Âu Ất Ấu Ma Mạc Mai Cao Chu Châu")
	cwords = strings.Fields("Công ty TNHH Cổ phần Thương mại Dịch vụ Đại Phát Đông Á Bình Minh Hòa Phú Sài Gòn Hà Nội Việt Nam Quốc tế Xuất nhập khẩu Vận tải Du lịch Đầu tư Xây dựng An Khang Thịnh Vượng Ánh Dương Ngôi Sao Ơn Trời")
	punct  = []string{"", "", "", " ", "-", ".", ",", "&", "(", "/", "'"}
	ascii  = strings.Fields("apple Apple APPLE banana Banana cherry date Date elder fig Fig grape honey ice Ice jam kiwi lemon Lemon mango nut oat pear quince rose sun Sun tea Tea uva vine wine xray yam zebra a A b B ab aB Ab AB a-b a_b a.b a b a1 a10 a2 1a 10 2 1 _x -y .z x- x_ x. X Y Z the The THE them then there o'neil oneil O'Neil o-neil co-op coop co op résumé resume Resume résume café cafe Cafe naïve naive zoo Zoo über uber Uber")
)

func gen(rng *rand.Rand, n int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	pick := func(w []string) string { return w[rng.Intn(len(w))] }
	for len(out) < n {
		var s string
		switch r := rng.Intn(10); {
		case r < 5:
			s = pick(family) + " " + pick(middle) + " " + pick(given)
		case r < 6:
			s = pick(family) + " " + pick(given)
		case r < 7: // "Given Family", "Family, Given"
			s = pick(family) + ", " + pick(middle) + " " + pick(given)
		case r < 9:
			s = "Công ty " + pick([]string{"TNHH", "CP", "Cổ phần"}) + " " + pick(cwords) + " " + pick(cwords)
			if rng.Intn(3) == 0 {
				s += fmt.Sprint(" ", rng.Intn(100))
			}
		default:
			s = pick(cwords) + pick(punct) + pick(cwords) + pick(punct) + fmt.Sprint(rng.Intn(50))
		}
		switch rng.Intn(8) {
		case 0:
			s = strings.ToLower(s)
		case 1:
			s = strings.ToUpper(s)
		case 2:
			s = strings.ReplaceAll(s, " ", "  ")
		case 3:
			s = " " + s
		case 4:
			s = strings.ReplaceAll(s, " ", "-")
		}
		add(s)
	}
	return out
}

func asciiCorpus() []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ascii {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func punctCorpus() []string {
	out := []string{"ab", "a b", "Ab", "AB", "aB"}
	for i := 33; i < 127; i++ {
		c := string(rune(i))
		if (c >= "a" && c <= "z") || (c >= "A" && c <= "Z") || (c >= "0" && c <= "9") {
			continue
		}
		out = append(out, "a"+c+"b", "a"+c, c+"a", "a"+c+c+"b", "A"+c+"b")
	}
	return out
}

func randomCorpus(rng *rand.Rand, n int) []string {
	alpha := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 -_.,'/&()@#$%+=<>àáảãạăắằẳẵặâấầẩẫậèéẻẽẹêếềểễệìíỉĩịòóỏõọôốồổỗộơớờởỡợùúủũụưứừửữựỳýỷỹỵđĐÀÁÂÃÈÉÊÌÍÒÓÔÕÙÚÝéüöäßñçåøæ€£")
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		l := 1 + rng.Intn(9)
		r := make([]rune, l)
		for i := range r {
			r[i] = alpha[rng.Intn(len(alpha))]
		}
		if !seen[string(r)] {
			seen[string(r)] = true
			out = append(out, string(r))
		}
	}
	return out
}

func crm(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.Trim(l, "\"")
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

var pgcollSQL string

func pgOrder(container string, vals []string) ([]int, error) {
	var in bytes.Buffer
	in.WriteString("DROP TABLE IF EXISTS cf; CREATE TABLE cf (id int, v text);\n\\copy cf FROM STDIN\n")
	for i, v := range vals {
		v = strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n").Replace(v)
		fmt.Fprintf(&in, "%d\t%s\n", i, v)
	}
	in.WriteString("\\.\n\\o /tmp/cf.out\nSELECT id FROM cf ORDER BY v" + pgcollSQL + ", id;\n")
	cmd := exec.Command("docker", "exec", "-i", container, "psql", "-U", "postgres", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1")
	cmd.Stdin = &in
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	out, err := exec.Command("docker", "exec", container, "cat", "/tmp/cf.out").Output()
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, l := range strings.Fields(string(out)) {
		var n int
		fmt.Sscan(l, &n)
		ids = append(ids, n)
	}
	return ids, nil
}

func main() {
	container := flag.String("container", "collation-pg", "")
	crmf := flag.String("crm", "/tmp/collation-crm-vals.txt", "")
	nn := flag.Int("n", 2000, "")
	show := flag.Int("show", 6, "")
	pgc := flag.String("pgcoll", "", "PG collation name for ORDER BY (default: database collation)")
	onlyEngine := flag.Bool("engine-only", false, "skip the pairwise variants")
	only := flag.String("only", "", "corpus name")
	onlyVar := flag.String("var", "", "variant name prefix")
	engColl := flag.String("engine", "", "also load the corpus into a STASK engine under this collation (en_US.utf8) and compare its ORDER BY")
	flag.Parse()
	if *pgc != "" {
		pgcollSQL = " COLLATE \"" + *pgc + "\""
	}
	rng := rand.New(rand.NewSource(1))
	names := gen(rng, *nn)
	corp := []struct {
		name string
		v    []string
	}{{"names2000", names}, {"crm", crm(*crmf)}, {"ascii", asciiCorpus()}, {"punct", punctCorpus()}, {"random5000", randomCorpus(rng, 5000)}}
	vars := variants()
	for _, c := range corp {
		if *only != "" && c.name != *only {
			continue
		}
		if len(c.v) == 0 {
			continue
		}
		want, err := pgOrder(*container, c.v)
		if err != nil {
			fmt.Println("pg:", err)
			os.Exit(1)
		}
		rank := make([]int, len(c.v))
		for r, id := range want {
			rank[id] = r
		}
		fmt.Printf("== corpus %s: %d distinct values\n", c.name, len(c.v))
		if *engColl != "" {
			got, err := engineOrder(c.v, *engColl)
			if err != nil {
				fmt.Println("engine:", err)
				os.Exit(1)
			}
			same := 0
			for i := range got {
				if got[i] == want[i] {
					same++
				}
			}
			fmt.Printf("ENGINE %-20s exact=%v positions_equal=%.2f%% rows=%d\n", *engColl, same == len(want) && len(got) == len(want), 100*float64(same)/float64(len(want)), len(got))
			if *onlyEngine {
				continue
			}
		}
		for _, v := range vars {
			if *onlyVar != "" && !strings.HasPrefix(v.name, *onlyVar) {
				continue
			}
			v.show = *onlyVar != ""
			idx := make([]int, len(c.v))
			for i := range idx {
				idx[i] = i
			}
			sort.SliceStable(idx, func(a, b int) bool { return v.cmp(c.v[idx[a]], c.v[idx[b]]) < 0 })
			exact, pos := true, 0
			for i := range idx {
				if idx[i] != want[i] {
					exact = false
					pos++
				}
			}
			// pairwise agreement over all pairs
			agree, total := 0, 0
			n := len(c.v)
			for i := 0; i < n; i++ {
				for j := i + 1; j < n; j++ {
					total++
					x := v.cmp(c.v[i], c.v[j])
					if (x < 0) == (rank[i] < rank[j]) {
						agree++
					}
				}
			}
			fmt.Printf("%-28s exact=%v positions_equal=%.2f%% pairs_agree=%.3f%%\n", v.name, exact, 100*float64(len(idx)-pos)/float64(len(idx)), 100*float64(agree)/float64(total))
			if *show > 0 && v.show {
				shown := 0
				for i := range idx {
					if idx[i] != want[i] && shown < *show {
						fmt.Printf("    pos %d: pg=%q stask=%q\n", i, c.v[want[i]], c.v[idx[i]])
						shown++
					}
				}
			}
		}
	}
	_ = sql.DefaultCollation
}
