// qisniff tries to assemble incoming tcp streams, and will warn you if any one of them contained packets
// with different payloads for the same segment of the stream.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"os"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/zond/qisniff/blocks"
)

var bars = []string{"-", "\\", "|", "/"}

var files []string

type diff struct {
	a   []byte
	b   []byte
	id  *streamID
	seq uint32
}

type diffs []diff

type streamID struct {
	srcIP   string
	dstIP   string
	srcPort layers.TCPPort
	dstPort layers.TCPPort
}

func (i streamID) String() string {
	return fmt.Sprintf("%v:%v->%v:%v", net.IP(i.srcIP), i.srcPort, net.IP(i.dstIP), i.dstPort)
}

type stream struct {
	id      *streamID
	f       *os.File
	offset  int64
	lastSeq uint32
	done    blocks.Blocks
	diffs   diffs
}

func newStream(id *streamID, seq uint32) (*stream, error) {
	f, err := ioutil.TempFile(os.TempDir(), "qisniff")
	if err != nil {
		return nil, err
	}
	files = append(files, f.Name())
	return &stream{
		id:      id,
		f:       f,
		offset:  -int64(seq),
		lastSeq: seq,
	}, nil
}

func (s *stream) write(tcp *layers.TCP) error {

	if (tcp.SYN || tcp.FIN) && len(tcp.Payload) == 0 {
		s.offset--
	}

	if s.lastSeq > (math.MaxUint32-math.MaxUint32/4) && tcp.Seq < math.MaxUint32/4 {
		s.offset += math.MaxUint32
	}

	a := s.offset + int64(tcp.Seq)
	b := a + int64(len(tcp.Payload))

	if b > a && (b-a != 1 || !tcp.ACK || tcp.Payload[0] != 0) {

		for _, overlap := range s.done.Overlaps(a, b) {
			previous := make([]byte, overlap.B-overlap.A)
			if _, err := s.f.Seek(overlap.A, 0); err != nil {
				return err
			}
			if _, err := s.f.Read(previous); err != nil {
				return err
			}
			relStart := int64(0)
			relEnd := int64(len(tcp.Payload))
			if overlap.A > a {
				relStart += overlap.A - a
			}
			if overlap.B-overlap.A < int64(len(tcp.Payload)) {
				relEnd = relStart + (overlap.B - overlap.A)
			}
			relPayload := tcp.Payload[relStart:relEnd]

			if bytes.Compare(previous, relPayload) != 0 {
				s.diffs = append(s.diffs, diff{
					a:   previous,
					b:   relPayload,
					id:  s.id,
					seq: tcp.Seq,
				})
			}
		}

		if _, err := s.f.Seek(a, 0); err != nil {
			return fmt.Errorf("Seek(%v, 0): %v", a, err)
		}

		if _, err := s.f.Write(tcp.Payload); err != nil {
			return fmt.Errorf("Write(%v): %v", tcp.Payload, err)
		}

		s.done = s.done.Add(a, b)

	}

	return nil
}

func removeFiles() {
	for _, file := range files {
		os.Remove(file)
	}
}

func main() {
	defer removeFiles()

	file := flag.String("file", "", "A file to parse")
	dev := flag.String("dev", "", "A dev to sniff")

	flag.Parse()

	if (*file == "" && *dev == "") || (*file != "" && *dev != "") {
		flag.Usage()
		os.Exit(1)
	}

	var (
		srcIP   net.IP
		dstIP   net.IP
		eth     layers.Ethernet
		ip4     layers.IPv4
		ip6     layers.IPv6
		tcp     layers.TCP
		payload gopacket.Payload
		decoded []gopacket.LayerType
		isTCP   bool
		handle  *pcap.Handle
		err     error
	)

	if *file != "" {
		if handle, err = pcap.OpenOffline(*file); err != nil {
			panic(err)
		}
	} else {
		if handle, err = pcap.OpenLive(*dev, 8196, true, pcap.BlockForever); err != nil {
			panic(err)
		}
	}

	source := gopacket.NewPacketSource(handle, handle.LinkType())
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip4, &ip6, &tcp, &payload)
	streams := map[streamID]*stream{}

	count := 0

	for pkt := range source.Packets() {
		fmt.Printf("\r%v ", bars[count%len(bars)])
		count++
		if err := parser.DecodeLayers(pkt.Data(), &decoded); err != nil {
			continue
		}
		isTCP = false
		for _, typ := range decoded {
			switch typ {
			case layers.LayerTypeIPv4:
				srcIP = ip4.SrcIP
				dstIP = ip4.DstIP
			case layers.LayerTypeIPv6:
				srcIP = ip6.SrcIP
				dstIP = ip6.DstIP
			case layers.LayerTypeTCP:
				isTCP = true
			}
		}
		if isTCP {
			id := &streamID{
				srcIP:   string(srcIP),
				dstIP:   string(dstIP),
				srcPort: tcp.SrcPort,
				dstPort: tcp.DstPort,
			}

			stream, found := streams[*id]
			if found || tcp.SYN {
				if tcp.SYN {
					if found {
						os.Remove(stream.f.Name())
					}
					if stream, err = newStream(id, tcp.Seq); err != nil {
						panic(err)
					}
					streams[*id] = stream
				}
				if err := stream.write(&tcp); err != nil {
					panic(err)
				}
			}
		}
	}
	fmt.Println()
	for _, stream := range streams {
		if len(stream.diffs) > 0 {
			for _, diff := range stream.diffs {
				fmt.Printf("%v %v\n<A>\n%s\n</A>\n<B>\n%s\n</B>\n", diff.id, diff.seq, diff.a, diff.b)
			}
		}
	}
}
