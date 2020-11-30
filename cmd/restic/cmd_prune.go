package main

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"

	"github.com/spf13/cobra"
)

var errorIndexIncomplete = errors.Fatal("index is not complete")
var errorPacksMissing = errors.Fatal("packs from index missing in repo")
var errorSizeNotMatching = errors.Fatal("pack size does not match calculated size from index")

var cmdPrune = &cobra.Command{
	Use:   "prune [flags]",
	Short: "Remove unneeded data from the repository",
	Long: `
The "prune" command checks the repository and removes data that is not
referenced and therefore not needed any more.

EXIT STATUS
===========

Exit status is 0 if the command was successful, and non-zero if there was any error.
`,
	DisableAutoGenTag: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPrune(pruneOptions, globalOptions)
	},
}

// PruneOptions collects all options for the cleanup command.
type PruneOptions struct {
	DryRun bool

	MaxUnused      string
	maxUnusedBytes func(used uint64) (unused uint64) // calculates the number of unused bytes after repacking, according to MaxUnused

	MaxRepackSize  string
	MaxRepackBytes uint64

	RepackCachableOnly bool
}

var pruneOptions PruneOptions

func init() {
	cmdRoot.AddCommand(cmdPrune)
	f := cmdPrune.Flags()
	f.BoolVarP(&pruneOptions.DryRun, "dry-run", "n", false, "do not modify the repository, just print what would be done")
	addPruneOptions(cmdPrune)
}

func addPruneOptions(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&pruneOptions.MaxUnused, "max-unused", "5%", "tolerate given `limit` of unused data (absolute value in bytes with suffixes k/K, m/M, g/G, t/T, a value in % or the word 'unlimited')")
	f.StringVar(&pruneOptions.MaxRepackSize, "max-repack-size", "", "maximum `size` to repack (allowed suffixes: k/K, m/M, g/G, t/T)")
	f.BoolVar(&pruneOptions.RepackCachableOnly, "repack-cacheable-only", false, "only repack packs which are cacheable")
}

func verifyPruneOptions(opts *PruneOptions) error {
	if len(opts.MaxRepackSize) > 0 {
		size, err := parseSizeStr(opts.MaxRepackSize)
		if err != nil {
			return err
		}
		opts.MaxRepackBytes = uint64(size)
	}

	maxUnused := strings.TrimSpace(opts.MaxUnused)
	if maxUnused == "" {
		return errors.Fatalf("invalid value for --max-unused: %q", opts.MaxUnused)
	}

	// parse MaxUnused either as unlimited, a percentage, or an absolute number of bytes
	switch {
	case maxUnused == "unlimited":
		opts.maxUnusedBytes = func(used uint64) uint64 {
			return math.MaxUint64
		}

	case strings.HasSuffix(maxUnused, "%"):
		maxUnused = strings.TrimSuffix(maxUnused, "%")
		p, err := strconv.ParseFloat(maxUnused, 64)
		if err != nil {
			return errors.Fatalf("invalid percentage %q passed for --max-unused: %v", opts.MaxUnused, err)
		}

		if p < 0 {
			return errors.Fatal("percentage for --max-unused must be positive")
		}

		if p >= 100 {
			return errors.Fatal("percentage for --max-unused must be below 100%")
		}

		opts.maxUnusedBytes = func(used uint64) uint64 {
			return uint64(p / (100 - p) * float64(used))
		}

	default:
		size, err := parseSizeStr(maxUnused)
		if err != nil {
			return errors.Fatalf("invalid number of bytes %q for --max-unused: %v", opts.MaxUnused, err)
		}

		opts.maxUnusedBytes = func(used uint64) uint64 {
			return uint64(size)
		}
	}

	return nil
}

func shortenStatus(maxLength int, s string) string {
	if len(s) <= maxLength {
		return s
	}

	if maxLength < 3 {
		return s[:maxLength]
	}

	return s[:maxLength-3] + "..."
}

func runPrune(opts PruneOptions, gopts GlobalOptions) error {
	err := verifyPruneOptions(&opts)
	if err != nil {
		return err
	}

	repo, err := OpenRepository(gopts)
	if err != nil {
		return err
	}

	lock, err := lockRepoExclusive(gopts.ctx, repo)
	defer unlockRepo(lock)
	if err != nil {
		return err
	}

	return runPruneWithRepo(opts, gopts, repo, restic.NewIDSet())
}

func runPruneWithRepo(opts PruneOptions, gopts GlobalOptions, repo *repository.Repository, ignoreSnapshots restic.IDSet) error {
	// we do not need index updates while pruning!
	repo.DisableAutoIndexUpdate()

	if repo.Cache == nil {
		Print("warning: running prune without a cache, this may be very slow!\n")
	}

	Verbosef("loading indexes...\n")
	err := repo.LoadIndex(gopts.ctx)
	if err != nil {
		return err
	}

	usedBlobs, err := getUsedBlobs(gopts, repo, ignoreSnapshots)
	if err != nil {
		return err
	}

	plan, stats, err := planPrune(opts, gopts, repo, usedBlobs)
	if err != nil {
		return err
	}

	err = printPruneStats(gopts, stats)
	if err != nil {
		return err
	}

	return doPrune(opts, gopts, repo, plan)
}

type blobStats struct {
	Used         uint `json:"used"`
	Duplicate    uint `json:"duplicate"`
	Unused       uint `json:"unused"`
	Total        uint `json:"total"`
	Repack       uint `json:"repack"`
	RepackRm     uint `json:"repack_remove"`
	Remove       uint `json:"remove"`
	RemoveTotal  uint `json:"remove_total"`
	Remain       uint `json:"remaining"`
	RemainUnused uint `json:"remaining_unused"`
}

type sizeStats struct {
	Used         uint64 `json:"used"`
	Duplicate    uint64 `json:"duplicate"`
	Unused       uint64 `json:"unused"`
	Unref        uint64 `json:"unreferenced"`
	Total        uint64 `json:"total"`
	Repack       uint64 `json:"repack"`
	RepackRm     uint64 `json:"repack_remove"`
	Remove       uint64 `json:"remove"`
	RemoveTotal  uint64 `json:"remove_total"`
	Remain       uint64 `json:"remaining"`
	RemainUnused uint64 `json:"remaining_unused"`
}

type packStats struct {
	Used        uint `json:"used"`
	Unused      uint `json:"unused"`
	PartlyUsed  uint `json:"partly_used"`
	Unref       uint `json:"unreferenced"`
	Total       uint `json:"total"`
	Keep        uint `json:"keep"`
	Repack      uint `json:"repack"`
	Remove      uint `json:"remove"`
	RemoveTotal uint `json:"remove_total"`
}

type pruneStats struct {
	MessageType string    `json:"message_type"` // "summary"
	Blobs       blobStats `json:"blobs"`
	Size        sizeStats `json:"bytes"`
	Packs       packStats `json:"packfiles"`
}

type prunePlan struct {
	removePacksFirst restic.IDSet   // packs to remove first (unreferenced packs)
	repackPacks      restic.IDSet   // packs to repack
	keepBlobs        restic.BlobSet // blobs to keep during repacking
	removePacks      restic.IDSet   // packs to remove
	ignorePacks      restic.IDSet   // packs to ignore when rebuilding the index
}

// planPrune selects which files to rewrite and which to delete and which blobs to keep.
// Also some summary statistics are returned.
// The map usedBlobs is modified in the process.
func planPrune(opts PruneOptions, gopts GlobalOptions, repo restic.Repository, usedBlobs restic.BlobSet) (plan prunePlan, stats pruneStats, err error) {
	type packInfo struct {
		usedBlobs      uint
		unusedBlobs    uint
		duplicateBlobs uint
		usedSize       uint64
		unusedSize     uint64
		tpe            restic.BlobType
	}

	type packInfoWithID struct {
		ID restic.ID
		packInfo
	}

	ctx := gopts.ctx

	Verbosef("searching used packs...\n")

	keepBlobs := restic.NewBlobSet()
	duplicateBlobs := restic.NewBlobSet()

	// iterate over all blobs in index to find out which blobs are duplicates
	for blob := range repo.Index().Each(ctx) {
		bh := blob.BlobHandle
		size := uint64(blob.Length)
		switch {
		case usedBlobs.Has(bh): // used blob, move to keepBlobs
			usedBlobs.Delete(bh)
			keepBlobs.Insert(bh)
			stats.Size.Used += size
			stats.Blobs.Used++
		case keepBlobs.Has(bh): // duplicate blob
			duplicateBlobs.Insert(bh)
			stats.Size.Duplicate += size
			stats.Blobs.Duplicate++
		default:
			stats.Size.Unused += size
			stats.Blobs.Unused++
		}
	}

	// Check if all used blobs have been found in index
	if len(usedBlobs) != 0 {
		Warnf("%v not found in the index\n\n"+
			"Integrity check failed: Data seems to be missing.\n"+
			"Will not start prune to prevent (additional) data loss!\n"+
			"Please report this error (along with the output of the 'prune' run) at\n"+
			"https://github.com/restic/restic/issues/new/choose", usedBlobs)
		return plan, stats, errorIndexIncomplete
	}

	indexPack := make(map[restic.ID]packInfo)

	// save computed pack header size
	for pid, hdrSize := range repo.Index().PackSize(ctx, true) {
		// initialize tpe with NumBlobTypes to indicate it's not set
		indexPack[pid] = packInfo{tpe: restic.NumBlobTypes, usedSize: uint64(hdrSize)}
	}

	// iterate over all blobs in index to generate packInfo
	for blob := range repo.Index().Each(ctx) {
		ip := indexPack[blob.PackID]

		// Set blob type if not yet set
		if ip.tpe == restic.NumBlobTypes {
			ip.tpe = blob.Type
		}

		// mark mixed packs with "Invalid blob type"
		if ip.tpe != blob.Type {
			ip.tpe = restic.InvalidBlob
		}

		bh := blob.BlobHandle
		size := uint64(blob.Length)
		switch {
		case duplicateBlobs.Has(bh): // duplicate blob
			ip.usedSize += size
			ip.duplicateBlobs++
		case keepBlobs.Has(bh): // used blob, not duplicate
			ip.usedSize += size
			ip.usedBlobs++
		default: // unused blob
			ip.unusedSize += size
			ip.unusedBlobs++
		}
		// update indexPack
		indexPack[blob.PackID] = ip
	}

	Verbosef("collecting packs for deletion and repacking\n")
	removePacksFirst := restic.NewIDSet()
	removePacks := restic.NewIDSet()
	repackPacks := restic.NewIDSet()

	var repackCandidates []packInfoWithID
	repackAllPacksWithDuplicates := true

	keep := func(p packInfo) {
		stats.Packs.Keep++
		if p.duplicateBlobs > 0 {
			repackAllPacksWithDuplicates = false
		}
	}

	// loop over all packs and decide what to do
	bar := newProgressMax(!gopts.Quiet, uint64(len(indexPack)), "packs processed")
	err = repo.List(ctx, restic.PackFile, func(id restic.ID, packSize int64) error {
		p, ok := indexPack[id]
		if !ok {
			// Pack was not referenced in index and is not used  => immediately remove!
			Verboseff("will remove pack %v as it is unused and not indexed\n", id.Str())
			removePacksFirst.Insert(id)
			stats.Size.Unref += uint64(packSize)
			return nil
		}

		if p.unusedSize+p.usedSize != uint64(packSize) &&
			!(p.usedBlobs == 0 && p.duplicateBlobs == 0) {
			// Pack size does not fit and pack is needed => error
			// If the pack is not needed, this is no error, the pack can
			// and will be simply removed, see below.
			Warnf("pack %s: calculated size %d does not match real size %d\nRun 'restic rebuild-index'.",
				id.Str(), p.unusedSize+p.usedSize, packSize)
			return errorSizeNotMatching
		}

		// statistics
		switch {
		case p.usedBlobs == 0 && p.duplicateBlobs == 0:
			stats.Packs.Unused++
		case p.unusedBlobs == 0:
			stats.Packs.Used++
		default:
			stats.Packs.PartlyUsed++
		}

		// decide what to do
		switch {
		case p.usedBlobs == 0 && p.duplicateBlobs == 0:
			// All blobs in pack are no longer used => remove pack!
			removePacks.Insert(id)
			stats.Blobs.Remove += p.unusedBlobs
			stats.Size.Remove += p.unusedSize

		case opts.RepackCachableOnly && p.tpe == restic.DataBlob:
			// if this is a data pack and --repack-cacheable-only is set => keep pack!
			keep(p)

		case p.unusedBlobs == 0 && p.duplicateBlobs == 0 && p.tpe != restic.InvalidBlob:
			// All blobs in pack are used and not duplicates/mixed => keep pack!
			keep(p)

		default:
			// all other packs are candidates for repacking
			repackCandidates = append(repackCandidates, packInfoWithID{ID: id, packInfo: p})
		}

		delete(indexPack, id)
		bar.Add(1)
		return nil
	})
	bar.Done()
	if err != nil {
		return plan, stats, err
	}

	// At this point indexPacks contains only missing packs!

	// missing packs that are not needed can be ignored
	ignorePacks := restic.NewIDSet()
	for id, p := range indexPack {
		if p.usedBlobs == 0 && p.duplicateBlobs == 0 {
			ignorePacks.Insert(id)
			stats.Blobs.Remove += p.unusedBlobs
			stats.Size.Remove += p.unusedSize
			delete(indexPack, id)
		}
	}

	if len(indexPack) != 0 {
		Warnf("The index references %d needed pack files which are missing from the repository:\n", len(indexPack))
		for id := range indexPack {
			Warnf("  %v\n", id)
		}
		return plan, stats, errorPacksMissing
	}
	if len(ignorePacks) != 0 {
		Warnf("Missing but unneeded pack files are referenced in the index, will be repaired\n")
		for id := range ignorePacks {
			Warnf("will forget missing pack file %v\n", id)
		}
	}

	// calculate limit for number of unused bytes in the repo after repacking
	maxUnusedSizeAfter := opts.maxUnusedBytes(stats.Size.Used)

	// Sort repackCandidates such that packs with highest ratio unused/used space are picked first.
	// This is equivalent to sorting by unused / total space.
	// Instead of unused[i] / used[i] > unused[j] / used[j] we use
	// unused[i] * used[j] > unused[j] * used[i] as uint32*uint32 < uint64
	// Morover duplicates and packs containing trees are sorted to the beginning
	sort.Slice(repackCandidates, func(i, j int) bool {
		pi := repackCandidates[i].packInfo
		pj := repackCandidates[j].packInfo
		switch {
		case pi.duplicateBlobs > 0 && pj.duplicateBlobs == 0:
			return true
		case pj.duplicateBlobs > 0 && pi.duplicateBlobs == 0:
			return false
		case pi.tpe != restic.DataBlob && pj.tpe == restic.DataBlob:
			return true
		case pj.tpe != restic.DataBlob && pi.tpe == restic.DataBlob:
			return false
		}
		return pi.unusedSize*pj.usedSize > pj.unusedSize*pi.usedSize
	})

	repack := func(id restic.ID, p packInfo) {
		repackPacks.Insert(id)
		stats.Blobs.Repack += p.unusedBlobs + p.duplicateBlobs + p.usedBlobs
		stats.Size.Repack += p.unusedSize + p.usedSize
		stats.Blobs.RepackRm += p.unusedBlobs
		stats.Size.RepackRm += p.unusedSize
	}

	for _, p := range repackCandidates {
		reachedUnusedSizeAfter := (stats.Size.Unused-stats.Size.Remove-stats.Size.RepackRm < maxUnusedSizeAfter)

		reachedRepackSize := false
		if opts.MaxRepackBytes > 0 {
			reachedRepackSize = stats.Size.Repack+p.unusedSize+p.usedSize > opts.MaxRepackBytes
		}

		switch {
		case reachedRepackSize:
			keep(p.packInfo)

		case p.duplicateBlobs > 0, p.tpe != restic.DataBlob:
			// repacking duplicates/non-data is only limited by repackSize
			repack(p.ID, p.packInfo)

		case reachedUnusedSizeAfter:
			// for all other packs stop repacking if tolerated unused size is reached.
			keep(p.packInfo)

		default:
			repack(p.ID, p.packInfo)
		}
	}

	// if all duplicates are repacked, print out correct statistics
	if repackAllPacksWithDuplicates {
		stats.Blobs.RepackRm += stats.Blobs.Duplicate
		stats.Size.RepackRm += stats.Size.Duplicate
	}

	// calculate totals for statistics
	stats.MessageType = "summary"
	stats.Blobs.Total = stats.Blobs.Used + stats.Blobs.Unused + stats.Blobs.Duplicate
	stats.Blobs.RemoveTotal = stats.Blobs.Remove + stats.Blobs.RepackRm
	stats.Blobs.Remain = stats.Blobs.Total - stats.Blobs.RemoveTotal
	stats.Size.Total = stats.Size.Used + stats.Size.Duplicate + stats.Size.Unused + stats.Size.Unref
	stats.Size.Unused = stats.Size.Duplicate + stats.Size.Unused
	stats.Size.RemoveTotal = stats.Size.Remove + stats.Size.RepackRm + stats.Size.Unref
	stats.Size.Remain = stats.Size.Total - stats.Size.RemoveTotal
	stats.Size.RemainUnused = stats.Size.Unused - stats.Size.Remove - stats.Size.RepackRm
	stats.Packs.Unref = uint(len(removePacksFirst))
	stats.Packs.Total = stats.Packs.Used + stats.Packs.PartlyUsed + stats.Packs.Unused + stats.Packs.Unref
	stats.Packs.Repack = uint(len(repackPacks))
	stats.Packs.Remove = uint(len(removePacks))
	stats.Packs.RemoveTotal = stats.Packs.Unref + stats.Packs.Remove

	plan.removePacksFirst = removePacksFirst
	plan.repackPacks = repackPacks
	plan.keepBlobs = keepBlobs
	plan.removePacks = removePacks
	plan.ignorePacks = ignorePacks

	return plan, stats, nil
}

// printPruneStats prints out the statistics
func printPruneStats(gopts GlobalOptions, stats pruneStats) error {
	if gopts.JSON {
		return json.NewEncoder(gopts.stdout).Encode(stats)
	}

	Verboseff("\nused:        %10d blobs / %s\n", stats.Blobs.Used, formatBytes(stats.Size.Used))
	if stats.Blobs.Duplicate > 0 {
		Verboseff("duplicates:  %10d blobs / %s\n", stats.Blobs.Duplicate, formatBytes(stats.Size.Duplicate))
	}
	Verboseff("unused:      %10d blobs / %s\n", stats.Blobs.Unused, formatBytes(stats.Size.Unused))
	if stats.Size.Unref > 0 {
		Verboseff("unreferenced:                   %s\n", formatBytes(stats.Size.Unref))
	}

	Verboseff("total:       %10d blobs / %s\n", stats.Blobs.Total, formatBytes(stats.Size.Total))
	Verboseff("unused size: %s of total size\n", formatPercent(stats.Size.Unused, stats.Size.Total))

	Verbosef("\nto repack:   %10d blobs / %s\n", stats.Blobs.Repack, formatBytes(stats.Size.Repack))
	Verbosef("this removes %10d blobs / %s\n", stats.Blobs.RepackRm, formatBytes(stats.Size.RepackRm))
	Verbosef("to delete:   %10d blobs / %s\n", stats.Blobs.Remove, formatBytes(stats.Size.Remove+stats.Size.Unref))
	Verbosef("total prune: %10d blobs / %s\n", stats.Blobs.RemoveTotal, formatBytes(stats.Size.RemoveTotal))
	Verbosef("remaining:   %10d blobs / %s\n", stats.Blobs.Remain, formatBytes(stats.Size.Remain))

	Verbosef("unused size after prune: %s (%s of remaining size)\n",
		formatBytes(stats.Size.RemainUnused), formatPercent(stats.Size.RemainUnused, stats.Size.Remain))
	Verbosef("\n")
	Verboseff("totally used packs: %10d\n", stats.Packs.Used)
	Verboseff("partly used packs:  %10d\n", stats.Packs.PartlyUsed)
	Verboseff("unused packs:       %10d\n\n", stats.Packs.Unused)

	Verboseff("to keep:   %10d packs\n", stats.Packs.Keep)
	Verboseff("to repack: %10d packs\n", stats.Packs.Repack)
	Verboseff("to delete: %10d packs\n", stats.Packs.Remove)
	if stats.Packs.Unref > 0 {
		Verboseff("to delete: %10d unreferenced packs\n\n", stats.Packs.Unref)
	}
	return nil
}

// doPrune does the actual pruning:
// - remove unreferenced packs first
// - repack given pack files while keeping the given blobs
// - rebuild the index while ignoring all files that will be deleted
// - delete the files
// plan.removePacks and plan.ignorePacks are modified in this function.
func doPrune(opts PruneOptions, gopts GlobalOptions, repo restic.Repository, plan prunePlan) (err error) {
	ctx := gopts.ctx

	if opts.DryRun {
		if !gopts.JSON && gopts.verbosity >= 2 {
			if len(plan.removePacksFirst) > 0 {
				Printf("Would have removed the following unreferenced packs:\n%v\n\n", plan.removePacksFirst)
			}
			Printf("Would have repacked and removed the following packs:\n%v\n\n", plan.repackPacks)
			Printf("Would have removed the following no longer used packs:\n%v\n\n", plan.removePacks)
		}
		// Always quit here if DryRun was set!
		return nil
	}

	// unreferenced packs can be safely deleted first
	if len(plan.removePacksFirst) != 0 {
		Verbosef("deleting unreferenced packs\n")
		DeleteFiles(gopts, repo, plan.removePacksFirst, restic.PackFile)
	}

	if len(plan.repackPacks) != 0 {
		Verbosef("repacking packs\n")
		bar := newProgressMax(!gopts.Quiet, uint64(len(plan.repackPacks)), "packs repacked")
		_, err := repository.Repack(ctx, repo, plan.repackPacks, plan.keepBlobs, bar)
		bar.Done()
		if err != nil {
			return errors.Fatalf("%s", err)
		}

		// Also remove repacked packs
		plan.removePacks.Merge(plan.repackPacks)
	}

	if len(plan.ignorePacks) == 0 {
		plan.ignorePacks = plan.removePacks
	} else {
		plan.ignorePacks.Merge(plan.removePacks)
	}

	if len(plan.ignorePacks) != 0 {
		err = rebuildIndexFiles(gopts, repo, plan.ignorePacks, nil)
		if err != nil {
			return errors.Fatalf("%s", err)
		}
	}

	if len(plan.removePacks) != 0 {
		Verbosef("removing %d old packs\n", len(plan.removePacks))
		DeleteFiles(gopts, repo, plan.removePacks, restic.PackFile)
	}

	Verbosef("done\n")
	return nil
}

func rebuildIndexFiles(gopts GlobalOptions, repo restic.Repository, removePacks restic.IDSet, extraObsolete restic.IDs) error {
	Verbosef("rebuilding index\n")

	idx := (repo.Index()).(*repository.MasterIndex)
	packcount := uint64(len(idx.Packs(removePacks)))
	bar := newProgressMax(!gopts.Quiet, packcount, "packs processed")
	obsoleteIndexes, err := idx.Save(gopts.ctx, repo, removePacks, extraObsolete, bar)
	bar.Done()
	if err != nil {
		return err
	}

	Verbosef("deleting obsolete index files\n")
	return DeleteFilesChecked(gopts, repo, obsoleteIndexes, restic.IndexFile)
}

func getUsedBlobs(gopts GlobalOptions, repo restic.Repository, ignoreSnapshots restic.IDSet) (usedBlobs restic.BlobSet, err error) {
	ctx := gopts.ctx

	var snapshotTrees restic.IDs
	Verbosef("loading all snapshots...\n")
	err = restic.ForAllSnapshots(gopts.ctx, repo, ignoreSnapshots,
		func(id restic.ID, sn *restic.Snapshot, err error) error {
			debug.Log("add snapshot %v (tree %v, error %v)", id, *sn.Tree, err)
			if err != nil {
				return err
			}
			snapshotTrees = append(snapshotTrees, *sn.Tree)
			return nil
		})
	if err != nil {
		return nil, err
	}

	Verbosef("finding data that is still in use for %d snapshots\n", len(snapshotTrees))

	usedBlobs = restic.NewBlobSet()

	bar := newProgressMax(!gopts.Quiet, uint64(len(snapshotTrees)), "snapshots")
	defer bar.Done()

	err = restic.FindUsedBlobs(ctx, repo, snapshotTrees, usedBlobs, bar)
	if err != nil {
		if repo.Backend().IsNotExist(err) {
			return nil, errors.Fatal("unable to load a tree from the repo: " + err.Error())
		}

		return nil, err
	}
	return usedBlobs, nil
}
