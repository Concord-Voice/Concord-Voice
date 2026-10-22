package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseClientListExtractsOnlyClientIDAndUsername(t *testing.T) {
	raw := "id=8 addr=127.0.0.1:40000 laddr=127.0.0.1:6379 fd=12 name=cp-a age=3 idle=0 flags=N db=0 sub=0 psub=0 multi=-1 qbuf=0 qbuf-free=0 argv-mem=0 obl=0 oll=0 omem=0 tot-mem=0 events=r cmd=ping user=cv_cp_12 resp=2\n" +
		"id=9 addr=127.0.0.1:40001 laddr=127.0.0.1:6379 fd=13 name=mp-a age=5 idle=0 flags=N db=0 sub=0 psub=0 multi=-1 qbuf=0 qbuf-free=0 argv-mem=0 obl=0 oll=0 omem=0 tot-mem=0 events=r cmd=get user=cv_mp_11 resp=2\n"

	clients, err := parseClientList(raw)

	require.NoError(t, err)
	require.Len(t, clients, 2)
	require.Equal(t, "8", clients[0].ID)
	require.Equal(t, "cv_cp_12", clients[0].Username)
	require.Equal(t, "9", clients[1].ID)
	require.Equal(t, "cv_mp_11", clients[1].Username)
}

func TestParseClientListRejectsMalformedOrAmbiguousRows(t *testing.T) {
	for _, raw := range []string{
		"not-a-client-row\n",
		"id=8 addr=127.0.0.1:40000 user=cv_cp_12\nid=8 addr=127.0.0.1:40001 user=cv_cp_11\n",
		"id=8 addr=127.0.0.1:40000 user=cv_cp_12 user=cv_cp_11\n",
		"id=8 addr=127.0.0.1:40000\n",
	} {
		t.Run(strings.TrimSpace(raw), func(t *testing.T) {
			_, err := parseClientList(raw)
			require.Error(t, err)
		})
	}
}

func TestParseClientListAcceptsCompleteResponseBeyond64KiB(t *testing.T) {
	var raw strings.Builder
	for id := 1; id <= 1_000; id++ {
		fmt.Fprintf(&raw, "id=%d user=cv_cp_12 name=%s\n", id, strings.Repeat("a", 96))
	}
	require.Greater(t, raw.Len(), 64*1024)
	clients, err := parseClientList(raw.String())
	require.NoError(t, err)
	require.Len(t, clients, 1_000)
}
