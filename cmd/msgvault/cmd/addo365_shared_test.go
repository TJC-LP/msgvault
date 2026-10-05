package cmd

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

func saveO365Flags(t *testing.T) {
	t.Helper()
	savedGraph, savedAs, savedHeadless, savedTenant, savedNoDefault := o365Graph, o365As, o365Headless, o365TenantID, noDefaultIdentityAddO365
	t.Cleanup(func() {
		o365Graph, o365As, o365Headless, o365TenantID, noDefaultIdentityAddO365 = savedGraph, savedAs, savedHeadless, savedTenant, savedNoDefault
	})
}

// add-o365 --graph --as records the shared mailbox as the account and the
// signing-in user in its config. The scheduled sync then asks for a token
// with Mail.Read.Shared, saved under the mailbox's address.
func TestAddO365GraphSharedMailbox(t *testing.T) {
	saveO365Flags(t)
	assert, require := assert.New(t), require.New(t)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
		Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	const mailbox, user = "team@example.com", "user@example.com"

	add := func(args ...string) *store.Source {
		t.Helper()
		o365As = ""
		cmd := newAddO365LocalCmd()
		cmd.SetArgs(append([]string{mailbox, "--graph", "--" + oauthPreflightedFlag}, args...))
		require.NoError(cmd.ExecuteContext(ctx))
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		sources, err := st.ListSources(sourceTypeMSMail)
		require.NoError(err)
		require.Len(sources, 1, "re-adding reuses the account")
		assert.Equal(mailbox, sources[0].Identifier)
		return sources[0]
	}

	src := add("--as", user)
	mcfg, err := msmailConfigOf(src)
	require.NoError(err)
	assert.Equal(user, mcfg.SignedInAs)

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	mgr := microsoft.NewGraphMailManager(cfg.Microsoft.ClientID, "common", "", cfg.TokensDir(), testDiscardLogger())
	require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
	require.NoError(os.WriteFile(mgr.TokenPath(mailbox),
		[]byte(`{"access_token":"synthetic","refresh_token":"r","token_type":"Bearer","scopes":["https://graph.microsoft.com/Mail.Read"]}`), 0600))
	err = runScheduledMSMailSync(ctx, src, st, invocationFromContext(ctx))
	require.ErrorContains(err, "Mail.Read.Shared", "a shared mailbox needs the shared scope")

	src = add()
	mcfg, err = msmailConfigOf(src)
	require.NoError(err)
	assert.False(mcfg.shared(), "re-adding without --as makes it the user's own mailbox again")

	src = add("--as", mailbox)
	mcfg, err = msmailConfigOf(src)
	require.NoError(err)
	assert.False(mcfg.shared(), "--as naming the mailbox itself is the own-mailbox case")
}

func TestAddO365AsRequiresGraph(t *testing.T) {
	saveO365Flags(t)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
		Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	o365Graph = false
	cmd := newAddO365LocalCmd()
	cmd.SetArgs([]string{"team@example.com", "--as", "user@example.com", "--" + oauthPreflightedFlag})
	require.ErrorContains(t, cmd.ExecuteContext(ctx), "--as requires --graph")
}

func TestMSMailConfigOf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config sql.NullString
		want   string
		bad    bool
	}{
		{"no config", sql.NullString{}, "", false},
		{"empty object", sql.NullString{String: `{}`, Valid: true}, "", false},
		{"shared", sql.NullString{String: `{"signed_in_as":"user@example.com"}`, Valid: true}, "user@example.com", false},
		{"corrupt", sql.NullString{String: `{`, Valid: true}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			c, err := msmailConfigOf(&store.Source{Identifier: "team@example.com", SyncConfig: tc.config})
			if tc.bad {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(tc.want, c.SignedInAs)
			assert.Equal(tc.want != "", c.shared())
		})
	}
}
