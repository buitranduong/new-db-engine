// staskroute: HTTP router in front of a leader and its replicas
// (docs/06 section 2a). Writes go to the leader, reads to the replicas
// with the session's last write LSN so they see their own writes.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"stask/internal/router"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	leader := flag.String("leader", "", "leader base URL, e.g. http://10.0.0.1:8080")
	nodes := flag.String("nodes", "", "comma-separated base URLs of the nodes of a Raft cluster (staskd -raft-id): requests follow the leader, which is found again after a failover")
	replicas := flag.String("replicas", "", "comma-separated replica base URLs")
	flag.Parse()
	if *nodes != "" {
		var ns []string
		for _, n := range strings.Split(*nodes, ",") {
			if n = strings.TrimSpace(n); n != "" {
				ns = append(ns, n)
			}
		}
		rt, err := router.NewCluster(ns...)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("routing %s to the leader of %v (now %s)", *addr, ns, rt.Leader())
		log.Fatal(http.ListenAndServe(*addr, rt))
	}
	if *leader == "" {
		log.Fatal("-leader or -nodes is required")
	}
	var reps []string
	for _, r := range strings.Split(*replicas, ",") {
		if r = strings.TrimSpace(r); r != "" {
			reps = append(reps, r)
		}
	}
	rt, err := router.New(*leader, reps...)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("routing %s: writes to %s, reads to %v", *addr, *leader, reps)
	log.Fatal(http.ListenAndServe(*addr, rt))
}
