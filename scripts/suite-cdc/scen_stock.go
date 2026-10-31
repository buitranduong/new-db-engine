package main

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func init() {
	register("j_stock", "a stock consumer (pg_recvlogical from the postgres:16 image) reads the stream", scenStock)
}

func scenStock(t *T) {
	n := startSingle(t, "j_stock")
	c := mustConn(t, n.URL())
	ex(t, c, `CREATE TABLE st (id bigint PRIMARY KEY, v text)`, `CREATE PUBLICATION pub FOR TABLE st`)
	host, port, _ := strings.Cut(n.PG, ":")
	name := fmt.Sprintf("cdcsuite-recvlogical-%d", n.Pid())
	common := []string{"pg_recvlogical", "-h", host, "-p", port, "-U", "app", "-d", "stask"}
	docker := func(args ...string) *exec.Cmd {
		return exec.Command("docker", append([]string{"run", "--rm", "--network", "host", "-e", "PGSSLMODE=disable", "postgres:16"}, args...)...)
	}
	// text output (test_decoding) is readable
	out, err := docker(append(append([]string{}, common...), "--slot", "rl", "--create-slot", "-P", "test_decoding")...).CombinedOutput()
	if err != nil {
		t.Gap("pg_recvlogical --create-slot against STASK failed: %s", firstLines(string(out), 3))
		return
	}
	ex(t, c, `INSERT INTO st VALUES (1, 'a')`, `BEGIN`, `UPDATE st SET v = 'b' WHERE id = 1`, `INSERT INTO st VALUES (2, 'c')`, `COMMIT`, `DELETE FROM st WHERE id = 1`)
	args := []string{"run", "-d", "--name", name, "--network", "host", "-e", "PGSSLMODE=disable", "postgres:16"}
	args = append(args, common...)
	args = append(args, "--slot", "rl", "--start", "-f", "-", "--fsync-interval", "1")
	if o, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Gap("pg_recvlogical --start could not be launched: %s", firstLines(string(o), 3))
		return
	}
	defer exec.Command("docker", "rm", "-f", name).Run()
	var logs string
	ok := waitFor(20*time.Second, func() bool {
		o, _ := exec.Command("docker", "logs", name).CombinedOutput()
		logs = string(o)
		return strings.Count(logs, "COMMIT") >= 3
	})
	t.Logf("pg_recvlogical output:\n%s", logs)
	if !ok {
		t.Gap("pg_recvlogical received %d of 3 transactions: %s", strings.Count(logs, "COMMIT"), firstLines(logs, 4))
		return
	}
	for _, w := range []string{"INSERT: id[bigint]:1 v[text]:'a'", "UPDATE: id[bigint]:1 v[text]:'b'", "INSERT: id[bigint]:2 v[text]:'c'", "DELETE: id[bigint]:1"} {
		t.Check(strings.Contains(logs, w), "pg_recvlogical output lacks %q", w)
	}
	// the pgoutput plugin through the same tool: the slot can be created and streamed (output is binary)
	o2, err := docker(append(append([]string{}, common...), "--slot", "rl_pgo", "--create-slot", "-P", "pgoutput")...).CombinedOutput()
	t.Check(err == nil, "pg_recvlogical --create-slot -P pgoutput: %s", firstLines(string(o2), 3))
}
