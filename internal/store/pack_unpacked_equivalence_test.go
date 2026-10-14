package store

// The unpacked-blob queries read attachment rows only for candidate hashes.
// This test holds them to the plain row-by-row definition below, which tests
// every attachment row against the pack index, on seeded random archives with
// mixed-case, NULL, empty, BLOB, and integer hash values.

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const rowByRowUnpackedContentSQL = `
		SELECT content_hash, storage_path, COALESCE(MAX(size), -1)
		FROM attachments
		WHERE content_hash IS NOT NULL AND content_hash != ''
		  AND storage_path IS NOT NULL AND storage_path != ''
		  AND LOWER(storage_path) NOT LIKE 'http://%'
		  AND LOWER(storage_path) NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = LOWER(attachments.content_hash))
		GROUP BY content_hash, storage_path
		ORDER BY MIN(id), storage_path`

const rowByRowUnpackedThumbnailSQL = `
		SELECT thumbnail_hash, thumbnail_path
		FROM attachments
		WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
		  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
		  AND LOWER(thumbnail_path) NOT LIKE 'http://%'
		  AND LOWER(thumbnail_path) NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = LOWER(attachments.thumbnail_hash))
		GROUP BY thumbnail_hash, thumbnail_path
		ORDER BY MIN(id), thumbnail_path`

func dumpUnpackedQuery(t *testing.T, st *Store, query string) []string {
	t.Helper()
	rows, err := st.db.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		var b strings.Builder
		for _, v := range vals {
			fmt.Fprintf(&b, "%T:%v|", v, v)
		}
		out = append(out, b.String())
	}
	require.NoError(t, rows.Err())
	return out
}

func TestUnpackedBlobQueriesMatchRowByRowDefinition(t *testing.T) {
	require := require.New(t)
	const iterations = 300
	dir := t.TempDir()
	st, err := OpenForTest(filepath.Join(dir, "equiv.db"))
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	require.NoError(st.InitSchema())
	ctx := t.Context()
	// One connection, so the pragma applies to every statement below.
	st.db.SetMaxOpenConns(1)
	_, err = st.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(err)

	hexHash := func(r *rand.Rand) string {
		const hexd = "0123456789abcdef"
		b := make([]byte, 64)
		for i := range b {
			b[i] = hexd[r.IntN(16)]
		}
		return string(b)
	}
	totalContent, totalThumb, nonEmpty := 0, 0, 0
	for iter := range iterations {
		r := rand.New(rand.NewPCG(uint64(iter), 0x5eed)) //nolint:gosec // Seeded so a failing iteration reproduces.
		_, err := st.db.ExecContext(ctx, `DELETE FROM attachments`)
		require.NoError(err)
		_, err = st.db.ExecContext(ctx, `DELETE FROM attachment_pack_index`)
		require.NoError(err)

		poolSize := 1 + r.IntN(40)
		pool := make([]string, poolSize)
		for i := range pool {
			pool[i] = hexHash(r)
		}
		hashValue := func() any {
			switch r.IntN(20) {
			case 0:
				return nil
			case 1:
				return ""
			case 2:
				return strings.ToUpper(pool[r.IntN(poolSize)])
			case 3: // mixed case
				h := []byte(pool[r.IntN(poolSize)])
				for i := range h {
					if r.IntN(2) == 0 {
						h[i] = strings.ToUpper(string(h[i]))[0]
					}
				}
				return string(h)
			case 4:
				return "not-a-hash"
			case 5:
				return " " + pool[r.IntN(poolSize)]
			case 6:
				return []byte(pool[r.IntN(poolSize)]) // BLOB-typed value
			case 7:
				return 12345
			default:
				return pool[r.IntN(poolSize)]
			}
		}
		pathValue := func(nullable bool) any {
			switch k := r.IntN(14); {
			case k == 0 && nullable:
				return nil
			case k == 1:
				return ""
			case k == 2:
				return "http://example.com/x"
			case k == 3:
				return "HTTPS://Example.com/y"
			case k == 4:
				return "hTtP://mixed"
			case k == 5:
				return "ftp://x"
			default:
				return fmt.Sprintf("%02x/p%d", r.IntN(4), r.IntN(6))
			}
		}
		rowsN := r.IntN(120)
		for i := range rowsN {
			var size any
			if r.IntN(5) != 0 {
				size = r.IntN(1000)
			}
			var thumbHash, thumbPath any
			if r.IntN(3) == 0 {
				thumbHash = hashValue()
				thumbPath = pathValue(true)
			}
			_, err := st.db.ExecContext(ctx, `INSERT INTO attachments
				(id, message_id, size, content_hash, storage_path, thumbnail_hash, thumbnail_path)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				1+i*(1+r.IntN(3)), 1+r.IntN(5), size, hashValue(), pathValue(false), thumbHash, thumbPath)
			if err != nil && strings.Contains(err.Error(), "UNIQUE") {
				continue
			}
			require.NoError(err)
		}
		for _, h := range pool {
			if r.IntN(2) == 0 {
				key := h
				if r.IntN(10) == 0 {
					key = strings.ToUpper(h) // non-canonical index row
				}
				_, err := st.db.ExecContext(ctx, `INSERT OR IGNORE INTO attachment_pack_index
					(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
					VALUES (?, 'p', 0, 1, 1, 0, 0)`, key)
				require.NoError(err)
			}
		}
		if r.IntN(4) == 0 {
			_, err := st.db.ExecContext(ctx, `INSERT OR IGNORE INTO attachment_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
				VALUES ('12345', 'p', 0, 1, 1, 0, 0), ('not-a-hash', 'p', 0, 1, 1, 0, 0)`)
			require.NoError(err)
		}

		wantContent := dumpUnpackedQuery(t, st, rowByRowUnpackedContentSQL)
		gotContent := dumpUnpackedQuery(t, st, listUnpackedContentBlobsSQL)
		require.Equal(wantContent, gotContent, "content query diverges at iteration %d", iter)
		wantThumbnail := dumpUnpackedQuery(t, st, rowByRowUnpackedThumbnailSQL)
		gotThumbnail := dumpUnpackedQuery(t, st, listUnpackedThumbnailBlobsSQL)
		require.Equal(wantThumbnail, gotThumbnail, "thumbnail query diverges at iteration %d", iter)
		totalContent += len(wantContent)
		totalThumb += len(wantThumbnail)
		if len(wantContent) > 0 && len(wantThumbnail) > 0 {
			nonEmpty++
		}
	}
	// Guard against a generator that stops producing rows to compare.
	require.Positive(totalContent)
	require.Positive(totalThumb)
	require.Positive(nonEmpty)
}
