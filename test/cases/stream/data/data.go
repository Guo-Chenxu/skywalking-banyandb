// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package data contains integration test cases of the stream.
package data

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	g "github.com/onsi/ginkgo/v2"
	gm "github.com/onsi/gomega"
	grpclib "google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
	modelv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/model/v1"
	streamv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/stream/v1"
	"github.com/apache/skywalking-banyandb/pkg/convert"
	"github.com/apache/skywalking-banyandb/pkg/test/flags"
	"github.com/apache/skywalking-banyandb/pkg/test/helpers"
)

//go:embed input/*.yaml
var inputFS embed.FS

//go:embed want/*.yaml
var wantFS embed.FS

//go:embed testdata/*.json
var dataFS embed.FS

// VerifyFn verify whether the query response matches the wanted result.
var VerifyFn = func(innerGm gm.Gomega, sharedContext helpers.SharedContext, args helpers.Args) {
	i, err := inputFS.ReadFile("input/" + args.Input + ".yaml")
	innerGm.Expect(err).NotTo(gm.HaveOccurred())
	query := &streamv1.QueryRequest{}
	helpers.UnmarshalYAML(i, query)
	query.TimeRange = helpers.TimeRange(args, sharedContext)
	query.Stages = args.Stages
	c := streamv1.NewStreamServiceClient(sharedContext.Connection)
	ctx := context.Background()
	resp, err := c.Query(ctx, query)
	if args.WantErr {
		if err == nil {
			g.Fail("expect error")
		}
		return
	}
	innerGm.Expect(err).NotTo(gm.HaveOccurred(), query.String())
	if args.WantEmpty {
		innerGm.Expect(resp.Elements).To(gm.BeEmpty())
		return
	}
	if args.Want == "" {
		args.Want = args.Input
	}
	ww, err := wantFS.ReadFile("want/" + args.Want + ".yaml")
	innerGm.Expect(err).NotTo(gm.HaveOccurred())
	want := &streamv1.QueryResponse{}
	helpers.UnmarshalYAML(ww, want)
	if !args.IgnoreElementID {
		for i := range want.Elements {
			want.Elements[i].ElementId = hex.EncodeToString(convert.Uint64ToBytes(convert.HashStr(query.Name + "|" + want.Elements[i].ElementId)))
		}
	}
	if args.DisOrder {
		slices.SortFunc(want.Elements, func(a, b *streamv1.Element) int {
			return strings.Compare(a.ElementId, b.ElementId)
		})
		slices.SortFunc(resp.Elements, func(a, b *streamv1.Element) int {
			return strings.Compare(a.ElementId, b.ElementId)
		})
	}
	var extra []cmp.Option
	extra = append(extra, protocmp.IgnoreUnknown(),
		protocmp.IgnoreFields(&streamv1.Element{}, "timestamp"),
		protocmp.Transform())
	if args.IgnoreElementID {
		extra = append(extra, protocmp.IgnoreFields(&streamv1.Element{}, "element_id"))
	}
	success := innerGm.Expect(cmp.Equal(resp, want,
		extra...)).
		To(gm.BeTrue(), func() string {
			var j []byte
			j, err = protojson.Marshal(resp)
			if err != nil {
				return err.Error()
			}
			var y []byte
			y, err = yaml.JSONToYAML(j)
			if err != nil {
				return err.Error()
			}
			return string(y)
		})
	if !success {
		return
	}
	query.Trace = true
	resp, err = c.Query(ctx, query)
	innerGm.Expect(err).NotTo(gm.HaveOccurred())
	innerGm.Expect(resp.Trace).NotTo(gm.BeNil())
	innerGm.Expect(resp.Trace.GetSpans()).NotTo(gm.BeEmpty())
}

func loadData(stream streamv1.StreamService_WriteClient, metadata *commonv1.Metadata, dataFile string, baseTime time.Time, interval time.Duration) {
	var templates []interface{}
	content, err := dataFS.ReadFile("testdata/" + dataFile)
	gm.Expect(err).ShouldNot(gm.HaveOccurred())
	gm.Expect(json.Unmarshal(content, &templates)).ShouldNot(gm.HaveOccurred())
	bb, _ := base64.StdEncoding.DecodeString("YWJjMTIzIT8kKiYoKSctPUB+")

	for i, template := range templates {
		rawSearchTagFamily, errMarshal := json.Marshal(template)
		gm.Expect(errMarshal).ShouldNot(gm.HaveOccurred())
		searchTagFamily := &modelv1.TagFamilyForWrite{}
		gm.Expect(protojson.Unmarshal(rawSearchTagFamily, searchTagFamily)).ShouldNot(gm.HaveOccurred())
		e := &streamv1.ElementValue{
			ElementId: strconv.Itoa(i),
			Timestamp: timestamppb.New(baseTime.Add(interval * time.Duration(i))),
			TagFamilies: []*modelv1.TagFamilyForWrite{
				{
					Tags: []*modelv1.TagValue{
						{
							Value: &modelv1.TagValue_BinaryData{
								BinaryData: bb,
							},
						},
					},
				},
			},
		}
		e.TagFamilies = append(e.TagFamilies, searchTagFamily)
		errInner := stream.Send(&streamv1.WriteRequest{
			Metadata:  metadata,
			Element:   e,
			MessageId: uint64(time.Now().UnixNano()),
		})
		gm.Expect(errInner).ShouldNot(gm.HaveOccurred())
	}
}

// Write data into the server.
func Write(conn *grpclib.ClientConn, name string, baseTime time.Time, interval time.Duration) {
	WriteToGroup(conn, name, "default", name, baseTime, interval)
}

// WriteToGroup data into the server with a specific group.
func WriteToGroup(conn *grpclib.ClientConn, name, group, fileName string, baseTime time.Time, interval time.Duration) {
	metadata := &commonv1.Metadata{
		Name:  name,
		Group: group,
	}
	schema := databasev1.NewStreamRegistryServiceClient(conn)
	resp, err := schema.Get(context.Background(), &databasev1.StreamRegistryServiceGetRequest{Metadata: metadata})
	gm.Expect(err).NotTo(gm.HaveOccurred())
	metadata = resp.GetStream().GetMetadata()

	c := streamv1.NewStreamServiceClient(conn)
	ctx := context.Background()
	writeClient, err := c.Write(ctx)
	gm.Expect(err).NotTo(gm.HaveOccurred())
	loadData(writeClient, metadata, fmt.Sprintf("%s.json", fileName), baseTime, interval)
	gm.Expect(writeClient.CloseSend()).To(gm.Succeed())
	gm.Eventually(func() error {
		_, err := writeClient.Recv()
		return err
	}, flags.EventuallyTimeout).Should(gm.Equal(io.EOF))
}
