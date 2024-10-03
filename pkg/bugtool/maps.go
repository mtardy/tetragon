// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Tetragon

package bugtool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"golang.org/x/exp/maps"
)

type ExtendedMapInfo struct {
	ebpf.MapInfo
	Memlock int
}

// TotalByteMemlock iterates over the extend map info and sums the memlock field.
func TotalByteMemlock(infos []ExtendedMapInfo) int {
	var sum int
	for _, info := range infos {
		sum += info.Memlock
	}
	return sum
}

// FindMapsUsedByPinnedProgs returns all info of maps used by the prog pinned
// under the path specified as argument. It also retrieve all the maps
// referenced in progs referenced in program array maps (tail calls).
func FindMapsUsedByPinnedProgs(path string) ([]ExtendedMapInfo, error) {
	mapIDs, err := mapIDsFromPinnedProgs(path)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving map IDs: %w", err)
	}
	mapInfos := []ExtendedMapInfo{}
	for _, mapID := range mapIDs {
		memlockInfo, err := memlockInfoFromMapID(mapID)
		if err != nil {
			return nil, fmt.Errorf("failed retrieving map memlock from ID: %w", err)
		}
		mapInfos = append(mapInfos, memlockInfo)
	}
	return mapInfos, nil
}

// FindAllMaps iterates over all maps loaded on the host using MapGetNextID and
// parses fdinfo to look for memlock.
func FindAllMaps() ([]ExtendedMapInfo, error) {
	var mapID ebpf.MapID
	var err error
	mapInfos := []ExtendedMapInfo{}

	for {
		mapID, err = ebpf.MapGetNextID(mapID)
		if err != nil {
			if errors.Is(err, syscall.ENOENT) {
				return mapInfos, nil
			}
			return nil, fmt.Errorf("didn't receive ENOENT at the end of iteration: %w", err)
		}

		m, err := ebpf.NewMapFromID(mapID)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve map from ID: %w", err)
		}
		defer m.Close()
		info, err := m.Info()
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve map info: %w", err)
		}

		memlock, err := parseMemlockFromFDInfo(m.FD())
		if err != nil {
			return nil, fmt.Errorf("failed parsing fdinfo to retrieve memlock: %w", err)
		}

		mapInfos = append(mapInfos, ExtendedMapInfo{
			MapInfo: *info,
			Memlock: memlock,
		})
	}
}

// FindPinnedMaps returns all info of maps pinned under the path
// specified as argument.
func FindPinnedMaps(path string) ([]ExtendedMapInfo, error) {
	var infos []ExtendedMapInfo
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, _ error) error {
		if d.IsDir() {
			return nil // skip directories
		}
		m, err := ebpf.LoadPinnedMap(path, &ebpf.LoadPinOptions{
			ReadOnly: true,
		})
		if err != nil {
			return fmt.Errorf("failed to load pinned map %q: %w", path, err)
		}
		defer m.Close()

		// check if it's really a map because ebpf.LoadPinnedMap does not return
		// an error but garbage info on doing this on a prog
		if ok, err := isMap(m.FD()); err != nil || !ok {
			if err != nil {
				return err
			}
			return nil // skip non map
		}

		info, err := m.Info()
		if err != nil {
			return fmt.Errorf("failed to retrieve map info: %w", err)
		}

		memlock, err := parseMemlockFromFDInfo(m.FD())
		if err != nil {
			return fmt.Errorf("failed to parse memlock from fd (%d) info: %w", m.FD(), err)
		}

		infos = append(infos, ExtendedMapInfo{
			MapInfo: *info,
			Memlock: memlock,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// mapIDsFromProgs retrieves all map IDs used inside a prog.
func mapIDsFromProgs(prog *ebpf.Program) ([]int, error) {
	if prog == nil {
		return nil, fmt.Errorf("prog is nil")
	}
	progInfo, err := prog.Info()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve prog info: %w", err)
	}
	// check if field is available
	ids, available := progInfo.MapIDs()
	if !available {
		return nil, fmt.Errorf("can't link prog to map IDs, field available from 4.15")
	}
	mapSet := map[int]bool{}
	for _, id := range ids {
		mapSet[int(id)] = true
	}
	return maps.Keys(mapSet), nil
}

// mapIDsFromPinnedProgs scan the given path and returns the map IDs used by the
// prog pinned under the path. It also retrieves the map IDs used by the prog
// referenced by program array maps (tail calls). This should only work from
// kernel 4.15.
func mapIDsFromPinnedProgs(path string) ([]int, error) {
	mapSet := map[int]bool{}
	progArrays := []*ebpf.Map{}
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, _ error) error {
		if d.IsDir() {
			return nil // skip directories
		}
		prog, err := ebpf.LoadPinnedProgram(path, &ebpf.LoadPinOptions{
			ReadOnly: true,
		})
		if err != nil {
			return fmt.Errorf("failed to load pinned object %q: %w", path, err)
		}
		defer prog.Close()

		if ok, err := isProg(prog.FD()); err != nil || !ok {
			if err != nil {
				return err
			}

			// we want to keep a ref to prog array containing tail calls to
			// search reference to map inside
			ok, err := isMap(prog.FD())
			if err != nil {
				return err
			}
			if ok {
				m, err := ebpf.LoadPinnedMap(path, &ebpf.LoadPinOptions{
					ReadOnly: true,
				})
				if err != nil {
					return fmt.Errorf("failed to load pinned map %q: %w", path, err)
				}
				if m.Type() == ebpf.ProgramArray {
					progArrays = append(progArrays, m)
					// don't forget to close those files when used later on
				} else {
					m.Close()
				}
			}

			return nil // skip the non-prog
		}

		newIDs, err := mapIDsFromProgs(prog)
		if err != nil {
			return fmt.Errorf("failed to retrieve map IDs from prog: %w", err)
		}
		for _, id := range newIDs {
			mapSet[id] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed walking the path %q: %w", path, err)
	}

	// retrieve all the program IDs from prog array maps
	progIDs := []int{}
	for _, progArray := range progArrays {
		if progArray == nil {
			return nil, fmt.Errorf("prog array reference is nil")
		}
		defer progArray.Close()

		var key, value uint32
		progArrayIterator := progArray.Iterate()
		for progArrayIterator.Next(&key, &value) {
			progIDs = append(progIDs, int(value))
			if err := progArrayIterator.Err(); err != nil {
				return nil, fmt.Errorf("failed to iterate over prog array map: %w", err)
			}
		}
	}

	// retrieve the map IDs from the prog array maps
	for _, progID := range progIDs {
		prog, err := ebpf.NewProgramFromID(ebpf.ProgramID(progID))
		if err != nil {
			return nil, fmt.Errorf("failed to create new program from id %d: %w", progID, err)
		}
		defer prog.Close()
		newIDs, err := mapIDsFromProgs(prog)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve map IDs from prog: %w", err)
		}
		for _, id := range newIDs {
			mapSet[id] = true
		}
	}

	return maps.Keys(mapSet), nil
}

func memlockInfoFromMapID(id int) (ExtendedMapInfo, error) {
	m, err := ebpf.NewMapFromID(ebpf.MapID(id))
	if err != nil {
		return ExtendedMapInfo{}, fmt.Errorf("failed creating a map FD from ID: %w", err)
	}
	defer m.Close()
	memlock, err := parseMemlockFromFDInfo(m.FD())
	if err != nil {
		return ExtendedMapInfo{}, fmt.Errorf("failed parsing fdinfo for memlock: %w", err)
	}
	info, err := m.Info()
	if err != nil {
		return ExtendedMapInfo{}, fmt.Errorf("failed retrieving info from map: %w", err)
	}

	return ExtendedMapInfo{
		MapInfo: *info,
		Memlock: memlock,
	}, nil
}

func parseMemlockFromFDInfo(fd int) (int, error) {
	path := fmt.Sprintf("/proc/self/fdinfo/%d", fd)
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("failed to open file %q: %w", path, err)
	}
	defer file.Close()
	return parseMemlockFromFDInfoReader(file)
}

func parseMemlockFromFDInfoReader(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 1 && fields[0] == "memlock:" {
			memlock, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("failed converting memlock to int: %w", err)
			}
			return memlock, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("failed to scan: %w", err)
	}
	return 0, fmt.Errorf("didn't find memlock field")
}

func isProg(fd int) (bool, error) {
	return isBPFObject("prog", fd)
}

func isMap(fd int) (bool, error) {
	return isBPFObject("map", fd)
}

func isBPFObject(object string, fd int) (bool, error) {
	readlink, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return false, fmt.Errorf("failed to readlink the fd (%d): %w", fd, err)
	}
	return readlink == fmt.Sprintf("anon_inode:bpf-%s", object), nil
}

const TetragonBPFFS = "/sys/fs/bpf/tetragon"

type DiffMap struct {
	ID         int    `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`
	KeySize    int    `json:"key_size,omitempty"`
	ValueSize  int    `json:"value_size,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
	Memlock    int    `json:"memlock,omitempty"`
}

type AggregatedMap struct {
	Name           string  `json:"name,omitempty"`
	Type           string  `json:"type,omitempty"`
	KeySize        int     `json:"key_size,omitempty"`
	ValueSize      int     `json:"value_size,omitempty"`
	MaxEntries     int     `json:"max_entries,omitempty"`
	Count          int     `json:"count,omitempty"`
	TotalMemlock   int     `json:"total_memlock,omitempty"`
	PercentOfTotal float64 `json:"percent_of_total,omitempty"`
}

type MapsChecksOutput struct {
	TotalByteMemlock struct {
		AllMaps         int `json:"all_maps,omitempty"`
		PinnedProgsMaps int `json:"pinned_progs_maps,omitempty"`
		PinnedMaps      int `json:"pinned_maps,omitempty"`
	} `json:"total_byte_memlock,omitempty"`

	MapsStats struct {
		PinnedProgsMaps int `json:"pinned_progs_maps,omitempty"`
		PinnedMaps      int `json:"pinned_maps,omitempty"`
		Inter           int `json:"inter,omitempty"`
		Exter           int `json:"exter,omitempty"`
		Union           int `json:"union,omitempty"`
		Diff            int `json:"diff,omitempty"`
	} `json:"maps_stats,omitempty"`

	DiffMaps []DiffMap `json:"diff_maps,omitempty"`

	AggregatedMaps []AggregatedMap `json:"aggregated_maps,omitempty"`
}

func RunMapsChecks() (*MapsChecksOutput, error) {
	// check that the bpffs exists and we have permissions
	_, err := os.Stat(TetragonBPFFS)
	if err != nil {
		return nil, fmt.Errorf("make sure tetragon is running and you have enough permissions: %w", err)
	}

	// retrieve map infos
	allMaps, err := FindAllMaps()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve all maps: %w", err)
	}
	pinnedProgsMaps, err := FindMapsUsedByPinnedProgs(TetragonBPFFS)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve maps used by pinned progs: %w", err)
	}
	pinnedMaps, err := FindPinnedMaps(TetragonBPFFS)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve pinned maps: %w", err)
	}

	var out MapsChecksOutput

	// BPF maps memory usage
	out.TotalByteMemlock.AllMaps = TotalByteMemlock(allMaps)
	out.TotalByteMemlock.PinnedProgsMaps = TotalByteMemlock(pinnedProgsMaps)
	out.TotalByteMemlock.PinnedMaps = TotalByteMemlock(pinnedMaps)

	// details on map distribution
	pinnedProgsMapsSet := map[int]ExtendedMapInfo{}
	for _, info := range pinnedProgsMaps {
		id, ok := info.ID()
		if !ok {
			return nil, errors.New("failed retrieving progs ID, need >= 4.13, kernel is too old")
		}
		pinnedProgsMapsSet[int(id)] = info
	}

	pinnedMapsSet := map[int]ExtendedMapInfo{}
	for _, info := range pinnedMaps {
		id, ok := info.ID()
		if !ok {
			return nil, errors.New("failed retrieving map ID, need >= 4.13, kernel is too old")
		}
		pinnedMapsSet[int(id)] = info
	}

	diff := diff(pinnedMapsSet, pinnedProgsMapsSet)
	union := union(pinnedMapsSet, pinnedProgsMapsSet)

	out.MapsStats.PinnedProgsMaps = len(pinnedProgsMapsSet)
	out.MapsStats.PinnedMaps = len(pinnedMaps)
	out.MapsStats.Inter = len(inter(pinnedMapsSet, pinnedProgsMapsSet))
	out.MapsStats.Exter = len(exter(pinnedMapsSet, pinnedProgsMapsSet))
	out.MapsStats.Union = len(union)
	out.MapsStats.Diff = len(diff)

	// details on diff maps
	for _, d := range diff {
		id, ok := d.ID()
		if !ok {
			return nil, errors.New("failed retrieving map ID, need >= 4.13, kernel is too old")
		}
		out.DiffMaps = append(out.DiffMaps, DiffMap{
			ID:         int(id),
			Name:       d.Name,
			Type:       d.Type.String(),
			KeySize:    int(d.KeySize),
			ValueSize:  int(d.ValueSize),
			MaxEntries: int(d.MaxEntries),
			Memlock:    d.Memlock,
		})
	}

	// aggregates maps total memory use
	aggregatedMapsSet := map[string]struct {
		ExtendedMapInfo
		count int
	}{}
	var total int
	for _, m := range union {
		total += m.Memlock
		if e, exist := aggregatedMapsSet[m.Name]; exist {
			e.Memlock += m.Memlock
			e.count++
			aggregatedMapsSet[m.Name] = e
		} else {
			aggregatedMapsSet[m.Name] = struct {
				ExtendedMapInfo
				count int
			}{m, 1}
		}
	}
	aggregatedMaps := maps.Values(aggregatedMapsSet)
	sort.Slice(aggregatedMaps, func(i, j int) bool {
		return aggregatedMaps[i].Memlock > aggregatedMaps[j].Memlock
	})

	for _, m := range aggregatedMaps {
		out.AggregatedMaps = append(out.AggregatedMaps, AggregatedMap{
			Name:           m.Name,
			Type:           m.Type.String(),
			KeySize:        int(m.KeySize),
			ValueSize:      int(m.ValueSize),
			MaxEntries:     int(m.MaxEntries),
			Count:          m.count,
			TotalMemlock:   m.Memlock,
			PercentOfTotal: float64(m.Memlock) / float64(total) * 100,
		})
	}

	return &out, nil
}

func inter[T any](m1, m2 map[int]T) map[int]T {
	ret := map[int]T{}
	for i := range m1 {
		if _, exists := m2[i]; exists {
			ret[i] = m1[i]
		}
	}
	return ret
}

func diff[T any](m1, m2 map[int]T) map[int]T {
	ret := map[int]T{}
	for i := range m1 {
		if _, exists := m2[i]; !exists {
			ret[i] = m1[i]
		}
	}
	return ret
}

func exter[T any](m1, m2 map[int]T) map[int]T {
	ret := map[int]T{}
	for i := range m1 {
		if _, exists := m2[i]; !exists {
			ret[i] = m1[i]
		}
	}
	for i := range m2 {
		if _, exists := m1[i]; !exists {
			ret[i] = m2[i]
		}
	}
	return ret
}

func union[T any](m1, m2 map[int]T) map[int]T {
	ret := map[int]T{}
	for i := range m1 {
		ret[i] = m1[i]
	}
	for i := range m2 {
		ret[i] = m2[i]
	}
	return ret
}