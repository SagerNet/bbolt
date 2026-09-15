package bbolt_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sagernet/bbolt"
	"github.com/sagernet/bbolt/internal/btesting"
	"github.com/sagernet/bbolt/internal/common"
	"github.com/sagernet/bbolt/internal/guts_cli"
)

func TestTx_Check_CorruptPage(t *testing.T) {
	bucketName := []byte("data")

	t.Log("Creating db file.")
	db := btesting.MustCreateDBWithOption(t, &bbolt.Options{PageSize: 4096})

	// Each page can hold roughly 20 key/values pair, so 100 such
	// key/value pairs will consume about 5 leaf pages.
	err := db.Fill(bucketName, 1, 100,
		func(tx int, k int) []byte { return []byte(fmt.Sprintf("%04d", k)) },
		func(tx int, k int) []byte { return make([]byte, 100) },
	)
	require.NoError(t, err)

	t.Log("Corrupting a random leaf page.")
	victimPageId, validPageIds := corruptRandomLeafPageInBucket(t, db.DB, bucketName)

	t.Log("Running consistency check.")
	vErr := db.View(func(tx *bbolt.Tx) error {
		var cErrs []error

		t.Log("Check corrupted page.")
		errChan := tx.Check(bbolt.WithPageId(uint64(victimPageId)))
		for cErr := range errChan {
			cErrs = append(cErrs, cErr)
		}
		require.Greater(t, len(cErrs), 0)

		t.Log("Check valid pages.")
		cErrs = cErrs[:0]
		for _, pgId := range validPageIds {
			errChan = tx.Check(bbolt.WithPageId(uint64(pgId)))
			for cErr := range errChan {
				cErrs = append(cErrs, cErr)
			}
			require.Equal(t, 0, len(cErrs))
		}
		return nil
	})
	require.NoError(t, vErr)
	t.Log("All check passed")

	// Manually close the db, otherwise the PostTestCleanup will
	// check the db again and accordingly fail the test.
	db.MustClose()
}

func TestTx_Check_WithNestBucket(t *testing.T) {
	parentBucketName := []byte("parentBucket")

	t.Log("Creating db file.")
	db := btesting.MustCreateDBWithOption(t, &bbolt.Options{PageSize: 4096})

	err := db.Update(func(tx *bbolt.Tx) error {
		pb, bErr := tx.CreateBucket(parentBucketName)
		if bErr != nil {
			return bErr
		}

		t.Log("put some key/values under the parent bucket directly")
		for i := 0; i < 10; i++ {
			k, v := fmt.Sprintf("%04d", i), fmt.Sprintf("value_%4d", i)
			if pErr := pb.Put([]byte(k), []byte(v)); pErr != nil {
				return pErr
			}
		}

		t.Log("create a nested bucket and put some key/values under the nested bucket")
		cb, bErr := pb.CreateBucket([]byte("nestedBucket"))
		if bErr != nil {
			return bErr
		}

		for i := 0; i < 2000; i++ {
			k, v := fmt.Sprintf("%04d", i), fmt.Sprintf("value_%4d", i)
			if pErr := cb.Put([]byte(k), []byte(v)); pErr != nil {
				return pErr
			}
		}

		return nil
	})
	require.NoError(t, err)

	// Get the bucket's root page.
	bucketRootPageId := mustGetBucketRootPage(t, db.DB, parentBucketName)

	t.Logf("Running consistency check starting from pageId: %d", bucketRootPageId)
	vErr := db.View(func(tx *bbolt.Tx) error {
		var cErrs []error

		errChan := tx.Check(bbolt.WithPageId(uint64(bucketRootPageId)))
		for cErr := range errChan {
			cErrs = append(cErrs, cErr)
		}
		require.Equal(t, 0, len(cErrs))

		return nil
	})
	require.NoError(t, vErr)
	t.Log("All check passed")

	// Manually close the db, otherwise the PostTestCleanup will
	// check the db again and accordingly fail the test.
	db.MustClose()
}

func TestTx_Check_Panic(t *testing.T) {
	bucketName := []byte("data")
	t.Log("Creating db file.")
	db := btesting.MustCreateDBWithOption(t, &bbolt.Options{PageSize: 4096})

	// Each page can hold roughly 20 key/values pair, so 100 such
	// key/value pairs will consume about 5 leaf pages.
	err := db.Fill(bucketName, 1, 100,
		func(tx int, k int) []byte { return []byte(fmt.Sprintf("%04d", k)) },
		func(tx int, k int) []byte { return make([]byte, 100) },
	)
	require.NoError(t, err)

	corruptRootPage(t, db.DB, bucketName)

	path := db.Path()

	require.NoError(t, db.Close())

	db = btesting.MustOpenDBWithOption(t, path, &bbolt.Options{PageSize: 4096})

	vErr := db.View(func(tx *bbolt.Tx) error {
		errChan := tx.Check()
		for cErr := range errChan {
			fmt.Println("cErr", cErr)
			return cErr
		}
		return nil
	})
	require.Error(t, vErr)
	require.ErrorContains(t, vErr, "has unexpected type/flags: 0")

	// Manually close the db, otherwise the PostTestCleanup will
	// check the db again and accordingly fail the test.
	db.MustClose()
}

func TestTx_Check_BucketCycle(t *testing.T) {
	options := &bbolt.Options{PageSize: 4096}
	path := createBucketCycle(t, options)

	db, err := bbolt.Open(path, 0600, options)
	require.NoError(t, err)
	defer db.Close()

	requireCheckReports(t, db, "multiple references")
}

func TestTx_Check_BranchPageCycle(t *testing.T) {
	bucketName := []byte("data")
	options := &bbolt.Options{PageSize: 4096}
	db := btesting.MustCreateDBWithOption(t, options)
	err := db.Fill(bucketName, 1, 100,
		func(tx int, k int) []byte { return []byte(fmt.Sprintf("%04d", k)) },
		func(tx int, k int) []byte { return make([]byte, 100) },
	)
	require.NoError(t, err)
	bucketRootPageId := mustGetBucketRootPage(t, db.DB, bucketName)
	path := db.Path()
	db.MustClose()

	t.Log("Pointing the last child of the bucket root page back at the root page.")
	bucketRootPage, bucketRootPageBuf, err := guts_cli.ReadPage(path, uint64(bucketRootPageId))
	require.NoError(t, err)
	require.True(t, bucketRootPage.IsBranchPage())
	bucketRootPage.BranchPageElement(bucketRootPage.Count() - 1).SetPgid(bucketRootPageId)
	require.NoError(t, guts_cli.WritePage(path, bucketRootPageBuf))

	corruptedDB, err := bbolt.Open(path, 0600, options)
	require.NoError(t, err)
	defer corruptedDB.Close()

	requireCheckReports(t, corruptedDB, "multiple references")
}

// createBucketCycle creates a db file in which a child bucket has the root page
// of its parent bucket as its own root page, so the bucket contains itself.
func createBucketCycle(t testing.TB, options *bbolt.Options) string {
	parentBucketName := []byte("parent")
	childBucketName := []byte("child")
	db := btesting.MustCreateDBWithOption(t, options)
	err := db.Update(func(tx *bbolt.Tx) error {
		parentBucket, bErr := tx.CreateBucket(parentBucketName)
		if bErr != nil {
			return bErr
		}
		childBucket, bErr := parentBucket.CreateBucket(childBucketName)
		if bErr != nil {
			return bErr
		}
		// Enough values to keep both buckets from being inlined, few enough
		// to keep each of them on a single leaf page.
		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("%04d", i))
			if pErr := parentBucket.Put(key, make([]byte, 100)); pErr != nil {
				return pErr
			}
			if pErr := childBucket.Put(key, make([]byte, 100)); pErr != nil {
				return pErr
			}
		}
		return nil
	})
	require.NoError(t, err)
	parentRootPageId := mustGetBucketRootPage(t, db.DB, parentBucketName)
	path := db.Path()
	db.MustClose()

	parentRootPage, parentRootPageBuf, err := guts_cli.ReadPage(path, uint64(parentRootPageId))
	require.NoError(t, err)
	require.True(t, parentRootPage.IsLeafPage())
	linked := false
	for i := uint16(0); i < parentRootPage.Count(); i++ {
		element := parentRootPage.LeafPageElement(i)
		if string(element.Key()) != string(childBucketName) {
			continue
		}
		require.True(t, element.IsBucketEntry())
		common.LoadBucket(element.Value()).SetRootPage(parentRootPageId)
		linked = true
	}
	require.True(t, linked)
	require.NoError(t, guts_cli.WritePage(path, parentRootPageBuf))
	return path
}

func requireCheckReports(t testing.TB, db *bbolt.DB, message string) {
	var cErrs []error
	err := db.View(func(tx *bbolt.Tx) error {
		for cErr := range tx.Check() {
			cErrs = append(cErrs, cErr)
		}
		return nil
	})
	require.NoError(t, err)
	for _, cErr := range cErrs {
		if strings.Contains(cErr.Error(), message) {
			return
		}
	}
	t.Fatalf("check did not report %q: %v", message, cErrs)
}

// corruptRandomLeafPage corrupts one random leaf page.
func corruptRandomLeafPageInBucket(t testing.TB, db *bbolt.DB, bucketName []byte) (victimPageId common.Pgid, validPageIds []common.Pgid) {
	bucketRootPageId := mustGetBucketRootPage(t, db, bucketName)
	bucketRootPage, _, err := guts_cli.ReadPage(db.Path(), uint64(bucketRootPageId))
	require.NoError(t, err)
	require.True(t, bucketRootPage.IsBranchPage())

	// Retrieve all the leaf pages included in the branch page, and pick up random one from them.
	var bucketPageIds []common.Pgid
	for _, bpe := range bucketRootPage.BranchPageElements() {
		bucketPageIds = append(bucketPageIds, bpe.Pgid())
	}
	randomIdx := rand.Intn(len(bucketPageIds))
	victimPageId = bucketPageIds[randomIdx]
	validPageIds = append(bucketPageIds[:randomIdx], bucketPageIds[randomIdx+1:]...)

	victimPage, victimBuf, err := guts_cli.ReadPage(db.Path(), uint64(victimPageId))
	require.NoError(t, err)
	require.True(t, victimPage.IsLeafPage())
	require.True(t, victimPage.Count() > 1)

	// intentionally make the second key < the first key.
	element := victimPage.LeafPageElement(1)
	key := element.Key()
	key[0] = 0

	// Write the corrupt page to db file.
	err = guts_cli.WritePage(db.Path(), victimBuf)
	require.NoError(t, err)
	return victimPageId, validPageIds
}

func corruptRootPage(t testing.TB, db *bbolt.DB, bucketName []byte) {
	bucketRootPageId := mustGetBucketRootPage(t, db, bucketName)
	bucketRootPage, bucketRootPageBuf, err := guts_cli.ReadPage(db.Path(), uint64(bucketRootPageId))
	require.NoError(t, err)
	require.True(t, bucketRootPage.IsBranchPage())

	bucketRootPage.SetFlags(0)

	err = guts_cli.WritePage(db.Path(), bucketRootPageBuf)
	require.NoError(t, err)
}

// mustGetBucketRootPage returns the root page for the provided bucket.
func mustGetBucketRootPage(t testing.TB, db *bbolt.DB, bucketName []byte) common.Pgid {
	var rootPageId common.Pgid
	_ = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		require.NotNil(t, b)
		rootPageId = b.RootPage()
		return nil
	})

	return rootPageId
}
