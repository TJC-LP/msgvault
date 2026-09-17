package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func saveImportOlmState(t *testing.T) func() {
	t.Helper()
	prevSourceType := importOlmSourceType
	prevSkipFolders := importOlmSkipFolders
	prevNoResume := importOlmNoResume
	prevCheckpointInterval := importOlmCheckpointInterval
	prevNoAttachments := importOlmNoAttachments
	prevNoDefaultIdentity := importOlmNoDefaultIdentity
	return func() {
		importOlmSourceType = prevSourceType
		importOlmSkipFolders = prevSkipFolders
		importOlmNoResume = prevNoResume
		importOlmCheckpointInterval = prevCheckpointInterval
		importOlmNoAttachments = prevNoAttachments
		importOlmNoDefaultIdentity = prevNoDefaultIdentity
	}
}

const olmCmdTestMessage = `<?xml version="1.0" encoding="UTF-8"?>
<emails><email>
<OPFMessageCopySubject>Hello</OPFMessageCopySubject>
<OPFMessageCopyMessageID>&lt;cmd1@example.com&gt;</OPFMessageCopyMessageID>
<OPFMessageCopySentTime>2023-03-04T05:06:07</OPFMessageCopySentTime>
<OPFMessageCopyFromAddresses><emailAddress OPFContactEmailAddressName="Sender" OPFContactEmailAddressAddress="sender@example.com"/></OPFMessageCopyFromAddresses>
<OPFMessageCopyToAddresses><emailAddress OPFContactEmailAddressName="Archive" OPFContactEmailAddressAddress="archive@example.com"/></OPFMessageCopyToAddresses>
<OPFMessageCopyBody>Body</OPFMessageCopyBody>
</email></emails>`

func writeCmdTestOlm(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "export.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: olmCmdTestMessage},
	})
	return p
}

func TestImportOlmCommandImportsAndConfirmsDefaultIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	t.Cleanup(saveImportOlmState(t))
	testCfg := lifecycleTestConfig(tmp)
	withStoreResolverConfig(t, testCfg)

	olmPath := writeCmdTestOlm(t, tmp)

	importOlmSourceType = "olm"
	importOlmNoResume = true
	importOlmCheckpointInterval = 200
	importOlmNoAttachments = true
	importOlmNoDefaultIdentity = false

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "import-olm"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)

	err := importOlmCmd.RunE(cmd, []string{"archive@example.com", olmPath})
	require.NoError(err, "import-olm")
	assert.Contains(stdout.String(), "Import complete.")
	assert.Contains(stdout.String(), "Added:          1 messages")

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	sources, err := st.GetSourcesByIdentifier("archive@example.com")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("olm", sources[0].SourceType)
	assert.Equal("export.olm", sources[0].DisplayName.String)

	ids, err := st.ListAccountIdentities(sources[0].ID)
	require.NoError(err)
	require.Len(ids, 1, "default identity confirmed from the account identifier")
	assert.Equal("archive@example.com", ids[0].Address)
	assert.Equal("account-identifier", ids[0].SourceSignal)

	var labels int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM labels WHERE source_id = ? AND name = 'Inbox'`, sources[0].ID).Scan(&labels))
	assert.Equal(1, labels, "folder became a label")
}

func TestImportOlmCommandNoDefaultIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	t.Cleanup(saveImportOlmState(t))
	testCfg := lifecycleTestConfig(tmp)
	withStoreResolverConfig(t, testCfg)

	olmPath := writeCmdTestOlm(t, tmp)

	importOlmSourceType = "olm"
	importOlmNoResume = true
	importOlmCheckpointInterval = 200
	importOlmNoAttachments = true
	importOlmNoDefaultIdentity = true

	cmd := &cobra.Command{Use: "import-olm"}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	require.NoError(importOlmCmd.RunE(cmd, []string{"archive@example.com", olmPath}))

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	sources, err := st.GetSourcesByIdentifier("archive@example.com")
	require.NoError(err)
	require.Len(sources, 1)
	ids, err := st.ListAccountIdentities(sources[0].ID)
	require.NoError(err)
	assert.Empty(ids)
}

func TestImportOlmCommandRejectsMissingFile(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	t.Cleanup(saveImportOlmState(t))
	withStoreResolverConfig(t, lifecycleTestConfig(tmp))
	importOlmNoResume = true

	cmd := &cobra.Command{Use: "import-olm"}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := importOlmCmd.RunE(cmd, []string{"archive@example.com", filepath.Join(tmp, "missing.olm")})
	require.Error(t, err)
}
