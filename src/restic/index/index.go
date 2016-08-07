// Package index contains various data structures for indexing content in a repository or backend.
package index

import (
	"fmt"
	"os"
	"restic/backend"
	"restic/debug"
	"restic/pack"
	"restic/repository"
	"restic/worker"
)

// Pack contains information about the contents of a pack.
type Pack struct {
	Size    int64
	Entries []pack.Blob
}

// Blob contains informaiton about a blob.
type Blob struct {
	Size  int64
	Packs backend.IDSet
}

// Index contains information about blobs and packs stored in a repo.
type Index struct {
	Packs map[backend.ID]Pack
	Blobs map[pack.Handle]Blob
}

func newIndex() *Index {
	return &Index{
		Packs: make(map[backend.ID]Pack),
		Blobs: make(map[pack.Handle]Blob),
	}
}

// New creates a new index for repo from scratch.
func New(repo *repository.Repository) (*Index, error) {
	done := make(chan struct{})
	defer close(done)

	ch := make(chan worker.Job)
	go repository.ListAllPacks(repo, ch, done)

	idx := newIndex()

	for job := range ch {
		packID := job.Data.(backend.ID)
		if job.Error != nil {
			fmt.Fprintf(os.Stderr, "unable to list pack %v: %v\n", packID.Str(), job.Error)
			continue
		}

		j := job.Result.(repository.ListAllPacksResult)

		debug.Log("Index.New", "pack %v contains %d blobs", packID.Str(), len(j.Entries))

		err := idx.AddPack(packID, j.Size, j.Entries)
		if err != nil {
			return nil, err
		}

		p := Pack{Entries: j.Entries, Size: j.Size}
		idx.Packs[packID] = p
	}

	return idx, nil
}

const loadIndexParallelism = 20

type packJSON struct {
	ID    backend.ID `json:"id"`
	Blobs []blobJSON `json:"blobs"`
}

type blobJSON struct {
	ID     backend.ID    `json:"id"`
	Type   pack.BlobType `json:"type"`
	Offset uint          `json:"offset"`
	Length uint          `json:"length"`
}

type indexJSON struct {
	Supersedes backend.IDs `json:"supersedes,omitempty"`
	Packs      []*packJSON `json:"packs"`
}

func loadIndexJSON(repo *repository.Repository, id backend.ID) (*indexJSON, error) {
	debug.Log("index.loadIndexJSON", "process index %v\n", id.Str())

	var idx indexJSON
	err := repo.LoadJSONUnpacked(backend.Index, id, &idx)
	if err != nil {
		return nil, err
	}

	return &idx, nil
}

// Load creates an index by loading all index files from the repo.
func Load(repo *repository.Repository) (*Index, error) {
	debug.Log("index.Load", "loading indexes")

	done := make(chan struct{})
	defer close(done)

	supersedes := make(map[backend.ID]backend.IDSet)
	results := make(map[backend.ID]map[backend.ID]Pack)

	index := newIndex()

	for id := range repo.List(backend.Index, done) {
		debug.Log("index.Load", "Load index %v", id.Str())
		idx, err := loadIndexJSON(repo, id)
		if err != nil {
			return nil, err
		}

		res := make(map[backend.ID]Pack)
		supersedes[id] = backend.NewIDSet()
		for _, sid := range idx.Supersedes {
			debug.Log("index.Load", "  index %v supersedes %v", id.Str(), sid)
			supersedes[id].Insert(sid)
		}

		for _, jpack := range idx.Packs {
			entries := make([]pack.Blob, 0, len(jpack.Blobs))
			for _, blob := range jpack.Blobs {
				entry := pack.Blob{
					ID:     blob.ID,
					Type:   blob.Type,
					Offset: blob.Offset,
					Length: blob.Length,
				}
				entries = append(entries, entry)
			}

			if err = index.AddPack(jpack.ID, 0, entries); err != nil {
				return nil, err
			}
		}

		results[id] = res
	}

	for superID, list := range supersedes {
		for indexID := range list {
			debug.Log("index.Load", "  removing index %v, superseded by %v", indexID.Str(), superID.Str())
			delete(results, indexID)
		}
	}

	return index, nil
}

// AddPack adds a pack to the index. If this pack is already in the index, an
// error is returned.
func (idx *Index) AddPack(id backend.ID, size int64, entries []pack.Blob) error {
	if _, ok := idx.Packs[id]; ok {
		return fmt.Errorf("pack %v already present in the index", id.Str())
	}

	idx.Packs[id] = Pack{Size: size, Entries: entries}

	for _, entry := range entries {
		h := pack.Handle{ID: entry.ID, Type: entry.Type}
		if _, ok := idx.Blobs[h]; !ok {
			idx.Blobs[h] = Blob{
				Size:  int64(entry.Length),
				Packs: backend.NewIDSet(),
			}
		}

		idx.Blobs[h].Packs.Insert(id)
	}

	return nil
}

// DuplicateBlobs returns a list of blobs that are stored more than once in the
// repo.
func (idx *Index) DuplicateBlobs() (dups pack.BlobSet) {
	dups = pack.NewBlobSet()
	seen := pack.NewBlobSet()

	for _, p := range idx.Packs {
		for _, entry := range p.Entries {
			h := pack.Handle{ID: entry.ID, Type: entry.Type}
			if seen.Has(h) {
				dups.Insert(h)
			}
			seen.Insert(h)
		}
	}

	return dups
}

// PacksForBlobs returns the set of packs in which the blobs are contained.
func (idx *Index) PacksForBlobs(blobs pack.BlobSet) (packs backend.IDSet) {
	packs = backend.NewIDSet()

	for h := range blobs {
		blob, ok := idx.Blobs[h]
		if !ok {
			continue
		}

		for id := range blob.Packs {
			packs.Insert(id)
		}
	}

	return packs
}