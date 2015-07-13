package dict

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"sort"

	"camlistore.org/pkg/rollsum"

	_ "github.com/mattn/go-sqlite3"
)

const (
	sqlUpSert = `
	INSERT OR REPLACE INTO chunks VALUES (
		?,
		?,
		COALESCE(1 + (SELECT count FROM chunks WHERE hash = ?), 1)
	);`
)

type Dict struct {
	db *sql.DB

	sdchDictChunks [][]byte

	// stats
	totalBytesDup uint64
	totalBytesIn  uint64
}

func New() (*Dict, error) {
	db, err := sql.Open("sqlite3", "dict")
	if err != nil {
		return nil, err
	}

	if err = db.Ping(); err != nil {
		return nil, err
	}

	_, err = db.Exec(
		`
CREATE TABLE IF NOT EXISTS chunks (
		content BLOB,
		hash BLOB UNIQUE ON CONFLICT REPLACE,
		count INTEGER
);`)
	if err != nil {
		return nil, err
	}

	return &Dict{
		db: db,
	}, nil
}

func (d *Dict) Eat(content []byte) (diff []byte, err error) {

	var diffBuf bytes.Buffer
	cmd := exec.Command("vcdiff", "delta", "-dictionary", "dictraw", "-interleaved", "-stats", "-checksum")
	cmd.Stdin = bytes.NewReader(content)
	cmd.Stdout = &diffBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	diff = diffBuf.Bytes()

	go func() {
		err := d.parse(content)
		if err != nil {
			log.Println("Error parsing:", err)
		}
	}()

	return diff, nil
}

func (d *Dict) parse(content []byte) error {
	rs := rollsum.New()

	var match uint64
	q := content
	buf := make([]byte, 0)
	hashes := make([][]byte, 0)
	offs := make([]int, 0)

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(sqlUpSert)
	if err != nil {
		return err
	}

	off := 0
	for len(q) > 0 {
		b := q[0]
		q = q[1:]

		rs.Roll(b)
		off++
		d.totalBytesIn++

		buf = append(buf, b)
		if rs.OnSplitWithBits(5) {
			h := sha1.Sum(buf)
			offs = append(offs, off)
			hashes = append(hashes, h[:])

			_, err := stmt.Exec(buf, h[:], h[:])
			if err != nil {
				return err
			}
			buf = buf[:0]
		}
	}

	d.totalBytesDup += uint64(match)

	if errStmt := stmt.Close(); errStmt != nil {
		return err
	}

	if errTx := tx.Commit(); errTx != nil {
		return err
	}

	err = d.makeDict()
	if err != nil {
		return err
	}

	return nil
}

func (d *Dict) makeDict() error {
	contents, hashes, change := d.needToUpdate()
	if change {
		log.Println("Changing dict")
		err := ioutil.WriteFile("dictraw", contents, 0644)
		if err != nil {
			return err
		}
		d.sdchDictChunks = hashes
	}
	return nil
}

func (d *Dict) needToUpdate() (contents []byte, hashes [][]byte, change bool) {
	rows, err := d.db.Query(`SELECT hash, content FROM chunks WHERE COUNT > 1 ORDER BY count, hash DESC`)
	if err != nil {
		log.Println(err)
		return nil, nil, false
	}
	defer rows.Close()

	hashes = make([][]byte, 0)
	contents = make([]byte, 0)
	var hash, content []byte
	for rows.Next() {
		rows.Scan(&hash, &content)
		contents = append(contents, content...)
		hashes = append(hashes, hash)
		content = content[:0]
		hash = hash[:0]
	}
	if err := rows.Err(); err != nil {
		log.Println(err)
		return nil, nil, false
	}

	if d.sdchDictChunks == nil || len(d.sdchDictChunks) == 0 {
		d.sdchDictChunks = hashes
		return nil, nil, false
	}

	all := make([][]byte, len(d.sdchDictChunks)+len(hashes))
	copy(all, d.sdchDictChunks)
	copy(all[len(all):], hashes)
	sort.Sort(sliceslice(all))

	last := all[0]
	uniq := 0
	isDup := false
	for _, fromAll := range all[1:] {
		if bytes.Compare(last, fromAll) == 0 {
			isDup = true
			continue
		}
		if !isDup {
			uniq++
		}
		last = fromAll
	}

	return contents, hashes, float64(uniq)/float64(len(d.sdchDictChunks)) > float64(0.1)
}

func (d *Dict) Stats() string {
	return fmt.Sprintf("matched %d out of %d", d.totalBytesDup, d.totalBytesIn)
}

type sliceslice [][]byte

func (ss sliceslice) Len() int           { return len(ss) }
func (ss sliceslice) Less(i, j int) bool { return bytes.Compare(ss[i], ss[j]) < 0 }
func (ss sliceslice) Swap(i, j int)      { ss[i], ss[j] = ss[j], ss[i] }
