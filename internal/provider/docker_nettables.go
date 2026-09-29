package provider

import (
	"context"
	"fmt"
	"strings"
)

// netTablesMark starts each table in the helper's output. Everything before the first one is
// docker's own (a pull on first use), and is ignored.
const netTablesMark = "@@sbx-nettable "

// netTablesScript prints the three tables, each after its mark. dev and tcp must exist; tcp6 is
// absent on a kernel with IPv6 off, which is not a failure.
const netTablesScript = `set -e
echo "` + netTablesMark + `dev"; cat /proc/net/dev
echo "` + netTablesMark + `tcp"; cat /proc/net/tcp
echo "` + netTablesMark + `tcp6"; cat /proc/net/tcp6 2>/dev/null || true`

// NetTables implements NetTabler: a throwaway alpine:3 - the helper snapshot and fork already
// run, and `sbx prewarm` pulls - joined to the container's network namespace. /proc/net is the
// reading process's namespace, so it lists the container's sockets and interfaces while needing
// nothing from its image. Unlabelled and --rm, so no listing ever sees it.
func (d *dockerProvider) NetTables(_ context.Context, ref string) (NetTables, error) {
	out, err := d.docker("run", "--rm", "-q", "--network", "container:"+ref, "--log-driver", "none",
		VolumeCopyImage, "sh", "-c", netTablesScript)
	if err != nil {
		return NetTables{}, err
	}

	return parseNetTables(out)
}

var _ NetTabler = (*dockerProvider)(nil)

func parseNetTables(out string) (NetTables, error) {
	tables := map[string]*strings.Builder{}

	var cur *strings.Builder

	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(line, netTablesMark); ok {
			cur = &strings.Builder{}
			tables[strings.TrimSpace(name)] = cur

			continue
		}

		if cur != nil {
			cur.WriteString(line + "\n")
		}
	}

	dev, tcp := tables["dev"], tables["tcp"]
	if dev == nil || tcp == nil {
		return NetTables{}, fmt.Errorf("the network-table helper printed no /proc/net/dev or tcp: %q", lastLines(out, 4))
	}

	t := NetTables{Dev: dev.String(), TCP: tcp.String()}
	if t6 := tables["tcp6"]; t6 != nil {
		t.TCP6 = t6.String()
	}

	return t, nil
}
