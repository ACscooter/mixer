// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/bigtable"
	pb "github.com/datacommonsorg/mixer/proto"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// BqPrefix for internal dataset.
	BqPrefix = "google.com:"
	// BtProject is the GCP project containing the BigTable instance
	BtProject = "google.com:datcom-store-dev"
	// BtInstance for internal cache instance.
	BtInstance = "prophet-cache"
	// BtPropValInPrefix for internal GetPropertyValues in arc cache.
	BtPropValInPrefix = "d/0/"
	// BtPropValOutPrefix for internal GetPropertyValues out arc cache.
	BtPropValOutPrefix = "d/1/"
	// BtPopObsPrefix for internal pop obs cache.
	BtPopObsPrefix = "d/2/"
	// BtPlaceObsPrefix for internal place obs cache.
	BtPlaceObsPrefix = "d/3/"
	// BtPlaceKMLPrefix for internal place KML coordinates cache.
	BtPlaceKMLPrefix = "d/6/"
	// BtTriplesPrefix for internal GetTriples cache.
	BtTriplesPrefix = "d/7/"
	// BtArcsPrefix for internal arcs cache.
	BtArcsPrefix = "d/9/"
	// BtPopPrefix for population cache.
	BtPopPrefix = "d/a/"
	// BtObsPrefix for observation cache.
	BtObsPrefix = "d/b/"
	// BtFamily is the key for the row.
	BtFamily = "csv"
	// BtCacheLimit is the cache limit. The limit is per predicate and neighbor type.
	BtCacheLimit = 200
	// BtBatchQuerySize is the size of BigTable batch query.
	BtBatchQuerySize = 1000
	// LimitFactor is the amount to multiply the limit by to make sure certain
	// triples are returned by the BQ query.
	LimitFactor = 1
	// TextType represents text type.
	TextType = "Text"
)

type typeInfo struct {
	Predicate string `json:"predicate"`
	SubType   string `json:"subType"`
	ObjType   string `json:"objType"`
}

// TypePair represents two types that are related.
type TypePair struct {
	Child  string
	Parent string
}

// GetProjectID gets the bigquery project id based on dataset name.
func GetProjectID(db string) (string, error) {
	if strings.HasPrefix(db, BqPrefix) {
		parts := strings.SplitN(strings.Replace(db, BqPrefix, "", 1), ".", 2)
		if len(parts) != 2 {
			return "", status.Errorf(
				codes.InvalidArgument, "Bad bigquery database name %s", db)
		}
		return BqPrefix + parts[0], nil
	}
	parts := strings.SplitN(db, ".", 2)
	if len(parts) != 2 {
		return "", status.Errorf(
			codes.InvalidArgument, "Bad bigquery database name %s", db)
	}
	return parts[0], nil
}

// ZipAndEncode Compresses the given contents using gzip and encodes it in base64
func ZipAndEncode(contents string) (string, error) {
	// Zip the string
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)

	_, err := gzWriter.Write([]byte(contents))
	if err != nil {
		return "", err
	}
	if err := gzWriter.Flush(); err != nil {
		return "", err
	}
	if err := gzWriter.Close(); err != nil {
		return "", err
	}

	// Encode using base64
	encode := base64.StdEncoding.EncodeToString(buf.Bytes())
	return encode, nil
}

// UnzipAndDecode decompresses the given contents using gzip and decodes it from base64
func UnzipAndDecode(contents string) ([]byte, error) {
	// Decode from base64
	decode, err := base64.StdEncoding.DecodeString(contents)
	if err != nil {
		return nil, err
	}

	// Unzip the string
	gzReader, err := gzip.NewReader(bytes.NewReader(decode))
	defer gzReader.Close()
	if err != nil {
		return nil, err
	}
	gzResult, err := ioutil.ReadAll(gzReader)
	if err != nil {
		return nil, err
	}

	return gzResult, nil
}

// StringList formats a list of strings into a comma-separated list with each surrounded
// with quotes.
func StringList(strs []string) string {
	strWithQuote := []string{}
	for _, s := range strs {
		strWithQuote = append(strWithQuote, fmt.Sprintf(`"%s"`, s))
	}
	return strings.Join(strWithQuote, ", ")
}

// StringContainedIn returns true if TARGET is contained in STRS
func StringContainedIn(target string, strs []string) bool {
	for _, s := range strs {
		if s == target {
			return true
		}
	}
	return false
}

// GetContainedIn returns the contained in relation change given two types.
func GetContainedIn(typeRelationJSONFilePath string) (map[TypePair][]string, error) {
	typeRelationJSON, err := ioutil.ReadFile(typeRelationJSONFilePath)
	if err != nil {
		return nil, err
	}

	ti := []typeInfo{}
	json.Unmarshal(typeRelationJSON, &ti)
	result := make(map[TypePair][]string)
	link := map[string][]string{}
	all := []string{}
	for _, info := range ti {
		if info.Predicate == "containedInPlace" {
			link[info.SubType] = append(link[info.SubType], info.ObjType)
			pair := TypePair{Child: info.SubType, Parent: info.ObjType}
			result[pair] = []string{}
			all = append(all, info.SubType, info.ObjType)
		}
	}
	// Node that has no parent node.
	endNodes := []string{}
	for _, one := range all {
		if _, ok := link[one]; !ok {
			endNodes = append(endNodes, one)
		}
	}
	for c, ps := range link {
		morep := ps
		for len(morep) > 0 {
			curr := morep[0]
			morep = morep[1:]
			for _, p := range link[curr] {
				result[TypePair{c, p}] = append(result[TypePair{c, curr}], curr)
				morep = append(morep, p)
			}
		}
	}
	return result, nil
}

// IterateSortPVs iterates a list of PVs and performs actions on them.
func IterateSortPVs(pvs []*pb.PropertyValue, action func(i int, p, v string)) {
	pvMap := map[string]string{}
	pList := []string{}
	for _, pv := range pvs {
		pvMap[pv.GetProperty()] = pv.GetValue()
		pList = append(pList, pv.GetProperty())
	}
	sort.Strings(pList)
	for i, p := range pList {
		action(i, p, pvMap[p])
	}
}

// SnakeToCamel converts a snake case string to camel case string.
func SnakeToCamel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}

	var result string
	parts := strings.Split(s, "_")
	capitalize := false
	for _, p := range parts {
		if p == "" {
			continue
		}

		if !capitalize {
			result += p
			capitalize = true
		} else {
			result += strings.Title(p)
		}
	}
	return result
}

var match = regexp.MustCompile("([a-z0-9])([A-Z])")

// CamelToSnake converts a camel case string to snake case string.
func CamelToSnake(str string) string {
	return strings.ToLower(match.ReplaceAllString(str, "${1}_${2}"))
}

// BigTableReadRowsParallel reads BigTable rows in parallel, considering the size limit for RowSet
// is 500KB.
func BigTableReadRowsParallel(ctx context.Context, btTable *bigtable.Table,
	rowSet bigtable.RowSet, action func(row bigtable.Row) error) error {
	var rowSetSize int
	var rowList bigtable.RowList
	var rowRangeList bigtable.RowRangeList

	switch v := rowSet.(type) {
	case bigtable.RowList:
		rowList = rowSet.(bigtable.RowList)
		rowSetSize = len(rowList)
	case bigtable.RowRangeList:
		rowRangeList = rowSet.(bigtable.RowRangeList)
		rowSetSize = len(rowRangeList)
	default:
		return fmt.Errorf("unsupported RowSet type: %v", v)
	}

	errs, errCtx := errgroup.WithContext(ctx)
	rowChan := make(chan []bigtable.Row, rowSetSize)
	for i := 0; i <= rowSetSize/BtBatchQuerySize; i++ {
		left := i * BtBatchQuerySize
		right := (i + 1) * BtBatchQuerySize
		if right > rowSetSize {
			right = rowSetSize
		}
		var rowSetPart bigtable.RowSet
		if len(rowList) > 0 {
			rowSetPart = rowList[left:right]
		} else {
			rowSetPart = rowRangeList[left:right]
		}

		errs.Go(func() error {
			btRows := []bigtable.Row{}
			if err := btTable.ReadRows(errCtx, rowSetPart,
				func(btRow bigtable.Row) bool {
					btRows = append(btRows, btRow)
					return true
				}); err != nil {
				return err
			}
			rowChan <- btRows
			return nil
		})
	}

	err := errs.Wait()
	if err != nil {
		return err
	}
	close(rowChan)

	for rows := range rowChan {
		for _, row := range rows {
			if err := action(row); err != nil {
				return err
			}
		}
	}

	return nil
}

// CheckValidDCIDs checks if DCIDs are valid. More criteria will be added as being discovered.
func CheckValidDCIDs(dcids []string) bool {
	for _, dcid := range dcids {
		if strings.Contains(dcid, " ") || strings.Contains(dcid, ",") {
			return false
		}
	}
	return true
}
