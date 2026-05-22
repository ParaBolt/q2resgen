package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	LUMP_ENTITIES = 0
	LUMP_TEXINFO  = 5
)

type Lump struct {
	Offset int32
	Length int32
}

type BSPHeader struct {
	Magic   [4]byte
	Version int32
	Lumps   [19]Lump
}

type TexInfo struct {
	Vecs        [2][4]float32
	Flags       int32
	Value       int32
	Texture     [32]byte
	NextTexInfo int32
}

// ---------------- CLI FLAGS ----------------

var (
	outDir     = flag.String("out", "", "Output directory (default: alongside BSP)")
	baseDir    = flag.String("basedir", "", "Quake2 base directory (for validation)")
	validate   = flag.Bool("validate", false, "Check if files exist in basedir")
	missingOut = flag.Bool("missing", false, "Write missing resources to .missing")
	recursive  = flag.Bool("r", false, "Scan directories recursively")
	ignoreFile = flag.String("ignore", "", "File listing resources to exclude (one per line)")
)

// ---------------- MAIN ----------------

func main() {
	// Allow flags anywhere in the argument list, not just before positional args.
	reorderArgs()
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Println("Usage: q2resgen [flags] <file|dir|*.bsp>")
		flag.PrintDefaults()
		return
	}

	var bspFiles []string

	for _, arg := range flag.Args() {
		files := collectBSPs(arg)
		bspFiles = append(bspFiles, files...)
	}

	if len(bspFiles) == 0 {
		fmt.Println("No BSP files found.")
		return
	}

	for _, bsp := range bspFiles {
		processBSP(bsp)
	}
}

// ---------------- FILE COLLECTION ----------------

func collectBSPs(input string) []string {
	var result []string

	info, err := os.Stat(input)
	if err != nil {
		// glob fallback
		matches, _ := filepath.Glob(input)
		return matches
	}

	if info.IsDir() {
		if *recursive {
			filepath.Walk(input, func(path string, info os.FileInfo, err error) error {
				if strings.HasSuffix(strings.ToLower(path), ".bsp") {
					result = append(result, path)
				}
				return nil
			})
		} else {
			files, _ := filepath.Glob(filepath.Join(input, "*.bsp"))
			result = append(result, files...)
		}
	} else {
		result = append(result, input)
	}

	return result
}

// ---------------- BSP PROCESSING ----------------

func processBSP(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer f.Close()

	var header BSPHeader
	if err := binary.Read(f, binary.LittleEndian, &header); err != nil {
		fmt.Println("Invalid BSP:", path)
		return
	}

	if string(header.Magic[:]) != "IBSP" || header.Version != 38 {
		fmt.Println("Skipping non-Q2 BSP:", path)
		return
	}

	resources := make(map[string]struct{})

	// --- ENTITIES ---
	readEntities(f, header.Lumps[LUMP_ENTITIES], resources)

	// --- TEXINFO ---
	readTexInfo(f, header.Lumps[LUMP_TEXINFO], resources)

	writeOutputs(path, resources)
}

// ---------------- ENTITY PARSER ----------------

func readEntities(f *os.File, lump Lump, res map[string]struct{}) {
	f.Seek(int64(lump.Offset), 0)

	data := make([]byte, lump.Length)
	f.Read(data)

	parseEntities(string(data), res)
}

func parseEntities(ent string, res map[string]struct{}) {
	kv := regexp.MustCompile(`"([^"]+)"\s+"([^"]+)"`)

	for _, m := range kv.FindAllStringSubmatch(ent, -1) {
		key := m[1]
		val := m[2]

		switch key {
		case "model":
			if !strings.HasPrefix(val, "*") {
				res[val] = struct{}{}
			}

		case "sound", "noise":
			if !strings.HasPrefix(val, "sound/") {
				val = "sound/" + val
			}
			res[val] = struct{}{}

		case "sky":
			for _, suffix := range []string{"rt", "lf", "up", "dn", "ft", "bk"} {
				res["env/"+val+suffix+".pcx"] = struct{}{}
			}
		}
	}
}

// ---------------- TEXINFO ----------------

func readTexInfo(f *os.File, lump Lump, res map[string]struct{}) {
	f.Seek(int64(lump.Offset), 0)

	count := int(lump.Length) / binary.Size(TexInfo{})

	for i := 0; i < count; i++ {
		var t TexInfo
		binary.Read(f, binary.LittleEndian, &t)

		name := strings.TrimRight(string(t.Texture[:]), "\x00")
		if name != "" {
			res["textures/"+name+".wal"] = struct{}{}
		}
	}
}

// ---------------- OUTPUT ----------------

func writeOutputs(bspPath string, res map[string]struct{}) {
	ignore := loadIgnoreList()

	var list []string
	for r := range res {
		n := normalize(r)
		if _, skip := ignore[n]; !skip {
			list = append(list, n)
		}
	}

	sort.Strings(list)

	outPath := buildOutPath(bspPath, ".res")

	outFile, _ := os.Create(outPath)
	defer outFile.Close()

	w := bufio.NewWriter(outFile)
	for _, r := range list {
		fmt.Fprintln(w, r)
	}
	w.Flush()

	fmt.Println("RES:", outPath)

	if *validate && *baseDir != "" {
		writeMissing(list, bspPath)
	}
}

func writeMissing(list []string, bspPath string) {
	var missing []string

	for _, r := range list {
		full := filepath.Join(*baseDir, r)
		if _, err := os.Stat(full); err != nil {
			missing = append(missing, r)
		}
	}

	if len(missing) == 0 {
		return
	}

	outPath := buildOutPath(bspPath, ".missing")

	f, _ := os.Create(outPath)
	defer f.Close()

	for _, m := range missing {
		fmt.Fprintln(f, m)
	}

	fmt.Println("Missing:", outPath)
}

// ---------------- IGNORE LIST ----------------

func loadIgnoreList() map[string]struct{} {
	ignore := make(map[string]struct{})
	if *ignoreFile == "" {
		return ignore
	}

	f, err := os.Open(*ignoreFile)
	if err != nil {
		fmt.Println("Warning: cannot open ignore file:", err)
		return ignore
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			ignore[normalize(line)] = struct{}{}
		}
	}
	return ignore
}

// ---------------- ARG REORDER ----------------

func reorderArgs() {
	var flags, positional []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// Check if this flag expects a value
				name := strings.TrimLeft(args[i], "-")
				if f := flag.Lookup(name); f != nil && f.DefValue != "false" && f.DefValue != "true" {
					i++
					flags = append(flags, args[i])
				}
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	os.Args = append([]string{os.Args[0]}, append(flags, positional...)...)
}

// ---------------- HELPERS ----------------

func normalize(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ToLower(p)
	return p
}

func buildOutPath(bsp, ext string) string {
	name := strings.TrimSuffix(filepath.Base(bsp), ".bsp") + ext

	if *outDir != "" {
		return filepath.Join(*outDir, name)
	}
	return filepath.Join(filepath.Dir(bsp), name)
}