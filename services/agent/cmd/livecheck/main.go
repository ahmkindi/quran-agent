// Command livecheck verifies the agent-service Gemini Live path WITHOUT a
// browser. It opens a VoiceBridge call, optionally streams an input WAV
// (16 kHz mono s16le), captures the agent's audio to an output WAV (24 kHz),
// and prints transcripts / tool events / byte counts.
//
// Tip: set AGENT_GREETING (e.g. "Greet the caller in English.") so the agent
// speaks first — then you can verify audio output with no input WAV at all.
//
// Usage:
//
//	go run ./services/agent/cmd/livecheck -addr localhost:9090 -secs 8 -out out.wav
//	go run ./services/agent/cmd/livecheck -in question_16k.wav -out out.wav
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	voicev1 "github.com/ahmkindi/quran-agent/gen/voice/v1"
)

func main() {
	addr := flag.String("addr", "localhost:9090", "agent-service gRPC address")
	in := flag.String("in", "", "optional input WAV (16 kHz mono s16le) to stream")
	out := flag.String("out", "out.wav", "output WAV path (24 kHz mono s16le)")
	secs := flag.Int("secs", 8, "seconds to listen for agent audio")
	stopAfter := flag.Duration("stop-after", 0, "send a recitation-control stop after this delay (0 = never); tests looping playback")
	flag.Parse()

	if err := run(*addr, *in, *out, *secs, *stopAfter); err != nil {
		fmt.Fprintln(os.Stderr, "livecheck:", err)
		os.Exit(1)
	}
}

func run(addr, inPath, outPath string, secs int, stopAfter time.Duration) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+10)*time.Second)
	defer cancel()

	stream, err := voicev1.NewVoiceBridgeClient(conn).Stream(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&voicev1.ClientFrame{Payload: &voicev1.ClientFrame_CallStart{
		CallStart: &voicev1.CallStart{CallId: "livecheck", SessionId: "livecheck"},
	}}); err != nil {
		return err
	}

	var outPCM []byte
	var audioFrames, interrupts int
	recvDone := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			switch p := f.Payload.(type) {
			case *voicev1.ServerFrame_AudioOut:
				audioFrames++
				outPCM = append(outPCM, p.AudioOut...)
			case *voicev1.ServerFrame_Interrupt:
				interrupts++
				fmt.Println("[interrupt]")
			case *voicev1.ServerFrame_Transcript:
				who := "agent"
				if p.Transcript.IsUser {
					who = "user"
				}
				fmt.Printf("[transcript %s%s] %s\n", who, finalMark(p.Transcript.Final), p.Transcript.Text)
			case *voicev1.ServerFrame_ToolEvent:
				fmt.Printf("[tool %s] %s\n", p.ToolEvent.Status, p.ToolEvent.Name)
			case *voicev1.ServerFrame_Hangup:
				fmt.Println("[hangup]", p.Hangup.Reason)
			}
		}
	}()

	if inPath != "" {
		pcm, sr, err := readWAV(inPath)
		if err != nil {
			return fmt.Errorf("read input wav: %w", err)
		}
		if sr != 16000 {
			fmt.Fprintf(os.Stderr, "warning: input is %d Hz; Gemini Live expects 16000 Hz\n", sr)
		}
		// Stream as 20 ms frames (16000 * 0.02 = 320 samples = 640 bytes).
		const frame = 640
		for off := 0; off < len(pcm); off += frame {
			end := min(off+frame, len(pcm))
			if err := stream.Send(&voicev1.ClientFrame{Payload: &voicev1.ClientFrame_AudioIn{AudioIn: pcm[off:end]}}); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond) // pace like real time
		}
		fmt.Printf("streamed %d bytes of input audio\n", len(pcm))
	}

	// Optionally act like the keyword spotter: halt playback mid-stream so
	// looping recitations can be verified headlessly.
	if stopAfter > 0 {
		go func() {
			time.Sleep(stopAfter)
			fmt.Println("[sending recitation-control stop]")
			_ = stream.Send(&voicev1.ClientFrame{Payload: &voicev1.ClientFrame_RecitationControl{
				RecitationControl: &voicev1.RecitationControl{Action: "stop"},
			}})
		}()
	}

	// Listen for the agent's audio.
	time.Sleep(time.Duration(secs) * time.Second)
	_ = stream.Send(&voicev1.ClientFrame{Payload: &voicev1.ClientFrame_CallEnd{CallEnd: &voicev1.CallEnd{Reason: "done"}}})
	_ = stream.CloseSend()

	select {
	case err := <-recvDone:
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
	case <-time.After(2 * time.Second):
	}

	if err := writeWAV(outPath, outPCM, 24000); err != nil {
		return fmt.Errorf("write output wav: %w", err)
	}
	fmt.Printf("\nresult: audio_frames=%d audio_bytes=%d interrupts=%d -> %s (%.1fs @24kHz)\n",
		audioFrames, len(outPCM), interrupts, outPath, float64(len(outPCM))/2/24000)
	if len(outPCM) == 0 {
		return errors.New("no audio received from agent (check GOOGLE_API_KEY / AGENT_GREETING / model name)")
	}
	return nil
}

func finalMark(final bool) string {
	if final {
		return ""
	}
	return "…"
}

// --- minimal WAV (PCM16 mono) helpers ---

func writeWAV(path string, pcm []byte, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	const bitsPerSample, channels = 16, 1
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)

	w := func(v any) { _ = binary.Write(f, binary.LittleEndian, v) }
	f.WriteString("RIFF")
	w(uint32(36 + dataLen))
	f.WriteString("WAVE")
	f.WriteString("fmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(byteRate))
	w(uint16(blockAlign))
	w(uint16(bitsPerSample))
	f.WriteString("data")
	w(uint32(dataLen))
	_, err = f.Write(pcm)
	return err
}

// readWAV parses a simple PCM16 WAV, returning samples and the sample rate.
func readWAV(path string) ([]byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a RIFF/WAVE file")
	}
	sampleRate := int(binary.LittleEndian.Uint32(b[24:28]))
	// Find the "data" chunk.
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		sz := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if id == "data" {
			end := min(body+sz, len(b))
			return b[body:end], sampleRate, nil
		}
		off = body + sz + (sz & 1) // chunks are word-aligned
	}
	return nil, 0, errors.New("no data chunk")
}
