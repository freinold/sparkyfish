package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"strconv"
	"syscall"
	"time"

	"gopkg.in/gizak/termui.v2"
)

// Kick off a throughput measurement test
func (sc *sparkyClient) runThroughputTest(testType command) {
	// Notify the progress bar updater to reset the bar
	sc.progressBarReset <- true

	// Used to signal test completion to the throughput measurer
	measurerDone := make(chan struct{})

	// Launch a throughput measurer and then kick off the metered copy,
	// blocking until it completes.
	go sc.MeasureThroughput(measurerDone)
	sc.MeteredCopy(testType, measurerDone)

	// Notify the progress bar updater that the test is done
	sc.testDone <- true
}

// Kicks off a metered copy (throughput test) by sending a command to the server
// and then performing the appropriate I/O copy, sending "ticks" by channel as
// each block of data passes through.
func (sc *sparkyClient) MeteredCopy(testType command, measurerDone chan<- struct{}) {
	var tl time.Duration

	// Connect to the remote sparkyfish server
	sc.beginSession()

	defer sc.conn.Close()

	// Send the appropriate command to the sparkyfish server to initiate our
	// throughput test
	switch testType {
	case inbound:
		// For inbound tests, we bump our timer by 2 seconds to account for
		// the remote server's test startup time
		tl = time.Second * time.Duration(throughputTestLength+2)

		// Send the SND command to the remote server, requesting a download test
		// (remote sends).
		err := sc.writeCommand("SND")
		if err != nil {
			termui.Close()
			log.Fatalln(err)
		}
	case outbound:
		tl = time.Second * time.Duration(throughputTestLength)

		// Send the RCV command to the remote server, requesting an upload test
		// (remote receives).
		err := sc.writeCommand("RCV")
		if err != nil {
			termui.Close()
			log.Fatalln(err)
		}
	}

	// Set a timer for running the tests
	timer := time.NewTimer(tl)

	switch testType {
	case inbound:
		// Receive, tally, and discard incoming data as fast as we can until the sender stops sending or the timer expires
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				// Copy data from our net.Conn to the rubbish bin in (blockSize) KB chunks
				_, err := io.CopyN(ioutil.Discard, sc.conn, 1024*blockSize)
				if err != nil {
					// Handle the EOF when the test timer has expired at the remote end.
					if err == io.EOF || err == io.ErrClosedPipe || err == syscall.EPIPE {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}
				// With each chunk copied, we send a message on our blockTicker channel
				sc.blockTicker <- true

			}
		}
	case outbound:
		// Send and tally outgoing data as fast as we can until the receiver stops receiving or the timer expires
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				// Copy data from our pre-filled bytes.Reader to the net.Conn in (blockSize) KB chunks
				_, err := io.CopyN(sc.conn, sc.randReader, 1024*blockSize)
				if err != nil {
					// If we get any of these errors, it probably just means that the server closed the connection
					if err == io.EOF || err == io.ErrClosedPipe || err == syscall.EPIPE {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}

				// Make sure that we have enough runway in our bytes.Reader to handle the next read
				if sc.randReader.Len() <= int(1024*blockSize) {
					// We're nearing the end of the Reader, so seek back to the beginning and start again
					sc.randReader.Seek(0, 0)
				}

				// With each chunk copied, we send a message on our blockTicker channel
				sc.blockTicker <- true
			}
		}
	}
}

// MeasureThroughput receives ticks sent by MeteredCopy() and derives a throughput rate, which is then sent
// to the throughput reporter.
func (sc *sparkyClient) MeasureThroughput(measurerDone <-chan struct{}) {
	var testType = inbound
	var blockCount, prevBlockCount uint64
	var throughput float64
	var throughputHist []float64

	tick := time.NewTicker(time.Duration(reportIntervalMS) * time.Millisecond)
	for {
		select {
		case <-sc.blockTicker:
			// Increment our block counter when we get a ticker
			blockCount++
		case <-measurerDone:
			tick.Stop()
			return
		case <-sc.changeToUpload:
			// The download test has completed, so we switch to tallying upload chunks
			testType = outbound
		case <-tick.C:
			throughput = (float64(blockCount - prevBlockCount)) * float64(blockSize*8) / float64(reportIntervalMS)

			// We discard the first element of the throughputHist slice once we have 70
			// elements stored.  This gives the user a chart that appears to scroll to
			// the left as new measurements come in and old ones are discarded.
			if len(throughputHist) >= 70 {
				throughputHist = throughputHist[1:]
			}

			// Add our latest measurement to the slice of historical measurements
			throughputHist = append(throughputHist, throughput)

			// Update the appropriate graph with the latest measurements
			switch testType {
			case inbound:
				sc.wr.jobs["dlgraph"].(*termui.LineChart).Data = throughputHist
			case outbound:
				sc.wr.jobs["ulgraph"].(*termui.LineChart).Data = throughputHist
			}

			// Send the latest measurement on to the stats generator
			sc.throughputReport <- throughput

			// Update the current block counter
			prevBlockCount = blockCount
		}
	}
}

// generateStats receives download and upload speed reports and computes metrics
// which are displayed in the stats widget.
func (sc *sparkyClient) generateStats() {
	var measurement float64
	var currentDL, maxDL, avgDL float64
	var currentUL, maxUL, avgUL float64
	var dlReadingCount, dlReadingSum float64
	var ulReadingCount, ulReadingSum float64
	var testType = inbound

	for {
		select {
		case measurement = <-sc.throughputReport:
			switch testType {
			case inbound:
				currentDL = measurement
				dlReadingCount++
				dlReadingSum = dlReadingSum + currentDL
				avgDL = dlReadingSum / dlReadingCount
				if currentDL > maxDL {
					maxDL = currentDL
				}
				// Update our stats widget with the latest readings
				sc.wr.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbit/s\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbit/s\tMax: %v\tAvg: %v",
					strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
					strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
				sc.wr.Render()
			case outbound:
				currentUL = measurement
				ulReadingCount++
				ulReadingSum = ulReadingSum + currentUL
				avgUL = ulReadingSum / ulReadingCount
				if currentUL > maxUL {
					maxUL = currentUL
				}
				// Update our stats widget with the latest readings
				sc.wr.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbit/s\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbit/s\tMax: %v\tAvg: %v",
					strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
					strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
				sc.wr.Render()

			}
		case <-sc.changeToUpload:
			testType = outbound
		case <-sc.statsGeneratorDone:
			return
		}
	}
}
