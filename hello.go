package hello

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	UptimeRoute    = "/uptime"
	WSRoute        = "/ws"
	SSERoute       = "/sse"
	HealthRoute    = "/_health"
	defaultSSEFreq = time.Second * 10
)

type OriginUpTime struct {
	StartTime time.Time `json:"startTime"`
	UpTime    string    `json:"uptime"`
}

type Snap struct {
	Time      int64          `json:"time"`
	TotalIPs  int            `json:"total_ips"`
	CPU       float64        `json:"cpu"`
	RAM       uint64         `json:"ram"`
	Countries map[string]int `json:"countries"`
}

type StatData struct {
	UptimeHours int            `json:"uptime_hours"`
	UptimeMins  int            `json:"uptime_mins"`
	TotalIPs    int            `json:"total_ips"`
	History     []Snap         `json:"history"`
	Cores       []float64      `json:"cores"`
	Countries   map[string]int `json:"countries"`
}

var (
	mu        sync.Mutex
	ips       = make(map[string]string)
	history   []Snap
	startTime time.Time

	prevTotalUser, prevTotalNice, prevTotalSys, prevTotalIdle uint64
	prevCores                                                 [][]uint64
)

func init() {
	startTime = time.Now()
	go statWorker()
}

func parseCPUFields(fields []string) (uint64, uint64, uint64, uint64) {
	if len(fields) < 5 {
		return 0, 0, 0, 0
	}
	u, _ := strconv.ParseUint(fields[1], 10, 64)
	n, _ := strconv.ParseUint(fields[2], 10, 64)
	s, _ := strconv.ParseUint(fields[3], 10, 64)
	i, _ := strconv.ParseUint(fields[4], 10, 64)
	return u, n, s, i
}

func getStats() (float64, []float64, uint64) {
	var totalCPU float64
	var cores []float64
	var ram uint64

	file, err := os.Open("/proc/stat")
	if err == nil {
		scanner := bufio.NewScanner(file)
		coreIdx := 0
		for scanner.Scan() {
			text := scanner.Text()
			fields := strings.Fields(text)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "cpu" {
				u, n, s, i := parseCPUFields(fields)
				total := u + n + s + i
				prevTotal := prevTotalUser + prevTotalNice + prevTotalSys + prevTotalIdle
				totalDiff := float64(total - prevTotal)
				if totalDiff > 0 {
					idleDiff := float64(i - prevTotalIdle)
					totalCPU = (totalDiff - idleDiff) / totalDiff * 100.0
				}
				prevTotalUser, prevTotalNice, prevTotalSys, prevTotalIdle = u, n, s, i
			} else if strings.HasPrefix(fields[0], "cpu") && len(fields[0]) > 3 {
				u, n, s, i := parseCPUFields(fields)
				total := u + n + s + i
				if coreIdx >= len(prevCores) {
					prevCores = append(prevCores, []uint64{0, 0, 0, 0})
				}
				prevU, prevN, prevS, prevI := prevCores[coreIdx][0], prevCores[coreIdx][1], prevCores[coreIdx][2], prevCores[coreIdx][3]
				prevTotal := prevU + prevN + prevS + prevI
				totalDiff := float64(total - prevTotal)
				coreLoad := 0.0
				if totalDiff > 0 {
					idleDiff := float64(i - prevI)
					coreLoad = (totalDiff - idleDiff) / totalDiff * 100.0
				}
				cores = append(cores, coreLoad)
				prevCores[coreIdx][0], prevCores[coreIdx][1], prevCores[coreIdx][2], prevCores[coreIdx][3] = u, n, s, i
				coreIdx++
			}
		}
		file.Close()
	}

	memFile, err := os.Open("/proc/meminfo")
	if err == nil {
		scanner := bufio.NewScanner(memFile)
		var totalMem, availMem uint64
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "MemTotal:") {
				fields := strings.Fields(text)
				totalMem, _ = strconv.ParseUint(fields[1], 10, 64)
			}
			if strings.HasPrefix(text, "MemAvailable:") {
				fields := strings.Fields(text)
				availMem, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		}
		memFile.Close()
		ram = (totalMem - availMem) * 1024
	}

	return totalCPU, cores, ram
}

func statWorker() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		mu.Lock()
		cpu, _, ram := getStats()
		cMap := make(map[string]int)
		for _, c := range ips {
			cMap[c]++
		}
		snap := Snap{
			Time:      time.Now().UnixMilli(),
			TotalIPs:  len(ips),
			CPU:       cpu,
			RAM:       ram,
			Countries: cMap,
		}
		history = append(history, snap)
		if len(history) > 300 {
			history = history[1:]
		}
		mu.Unlock()
	}
}

func StartHelloWorldServer(log *zerolog.Logger, listener net.Listener, shutdownC <-chan struct{}) error {
	log.Info().Msgf("Starting Hello World server at %s", listener.Addr())

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	muxer := http.NewServeMux()
	muxer.HandleFunc(UptimeRoute, uptimeHandler(startTime))
	muxer.HandleFunc(WSRoute, websocketHandler(log, upgrader))
	muxer.HandleFunc(SSERoute, sseHandler(log))
	muxer.HandleFunc(HealthRoute, healthHandler())

	muxer.HandleFunc("/api/update", apiUpdateHandler())
	muxer.HandleFunc("/api/stat", apiStatUIHandler())
	muxer.HandleFunc("/api/stat/data", apiStatDataHandler())

	httpServer := &http.Server{Addr: listener.Addr().String(), Handler: muxer}
	go func() {
		<-shutdownC
		_ = httpServer.Close()
	}()

	err := httpServer.Serve(listener)
	return err
}

func CreateTLSListener(address string) (net.Listener, error) {
	certificate, err := tlsconfig.GetHelloCertificate()
	if err != nil {
		return nil, err
	}

	listener, err := tls.Listen(
		"tcp",
		address,
		&tls.Config{Certificates: []tls.Certificate{certificate}})

	return listener, err
}

func uptimeHandler(startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := &OriginUpTime{StartTime: startTime, UpTime: time.Now().Sub(startTime).String()}
		respJson, err := json.Marshal(resp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(respJson)
		}
	}
}

func websocketHandler(log *zerolog.Logger, upgrader websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err == nil {
			r.Host = host
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Err(err).Msg("failed to upgrade to websocket connection")
			return
		}
		defer conn.Close()
		for {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				log.Err(err).Msg("websocket read message error")
				break
			}

			if err := conn.WriteMessage(mt, message); err != nil {
				log.Err(err).Msg("websocket write message error")
				break
			}
		}
	}
}

func sseHandler(log *zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			log.Error().Msgf("Can't support SSE. ResponseWriter %T doesn't implement http.Flusher interface", w)
			return
		}

		freq := defaultSSEFreq
		if requestedFreq := r.URL.Query()["freq"]; len(requestedFreq) > 0 {
			parsedFreq, err := time.ParseDuration(requestedFreq[0])
			if err == nil {
				freq = parsedFreq
			}
		}
		log.Info().Msgf("Server Sent Events every %s", freq)
		ticker := time.NewTicker(freq)
		counter := 0
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
			_, err := fmt.Fprintf(w, "%d\n\n", counter)
			if err != nil {
				return
			}
			flusher.Flush()
			counter++
		}
	}
}

func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}
}

func apiUpdateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("CF-Connecting-IP")
		if ip != "" {
			country := r.Header.Get("CF-IPCountry")
			if country == "" {
				country = "Unknown"
			}
			mu.Lock()
			ips[ip] = country
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message":"Hello, World!"}`))
	}
}

func apiStatDataHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		uptime := time.Since(startTime)
		hours := int(uptime.Hours())
		mins := int(uptime.Minutes()) % 60

		_, cores, _ := getStats()

		cMap := make(map[string]int)
		for _, c := range ips {
			cMap[c]++
		}

		histCopy := make([]Snap, len(history))
		copy(histCopy, history)

		data := StatData{
			UptimeHours: hours,
			UptimeMins:  mins,
			TotalIPs:    len(ips),
			History:     histCopy,
			Cores:       cores,
			Countries:   cMap,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}
}

const statHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Server Statistics</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
</head>
<body class="bg-gray-900 text-white font-sans min-h-screen p-8">
    <div class="max-w-6xl mx-auto space-y-6">
        <div class="flex justify-between items-center bg-gray-800 p-6 rounded-xl shadow-lg border border-gray-700">
            <div>
                <h1 class="text-3xl font-bold text-blue-400">Server Monitor</h1>
                <p class="text-sm text-gray-400 mt-1">Uptime: <span id="uptime" class="text-white font-mono">0 hours 0 minutes</span></p>
            </div>
            <div class="text-right">
                <p class="text-sm text-gray-400">Total Unique IPs</p>
                <p id="totalIps" class="text-4xl font-black text-green-400 font-mono">0</p>
            </div>
        </div>

        <div class="bg-gray-800 p-6 rounded-xl shadow-lg border border-gray-700">
            <h2 class="text-xl font-semibold mb-4 text-gray-200">System Analytics</h2>
            <div class="w-full h-96">
                <canvas id="mainChart"></canvas>
            </div>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 gap-6">
            <div class="bg-gray-800 p-6 rounded-xl shadow-lg border border-gray-700">
                <h2 class="text-xl font-semibold mb-4 text-gray-200">CPU Cores</h2>
                <div id="coresContainer" class="space-y-4"></div>
            </div>

            <div class="bg-gray-800 p-6 rounded-xl shadow-lg border border-gray-700">
                <h2 class="text-xl font-semibold mb-4 text-gray-200">IPs by Country</h2>
                <div id="countriesContainer" class="grid grid-cols-2 gap-4"></div>
            </div>
        </div>
    </div>

    <script>
        let chart;
        let rawHistory = [];

        function initChart() {
            const ctx = document.getElementById('mainChart').getContext('2d');
            chart = new Chart(ctx, {
                type: 'line',
                data: {
                    labels: [],
                    datasets: [
                        {
                            label: 'Total Unique IPs',
                            data: [],
                            borderColor: '#4ade80',
                            backgroundColor: 'rgba(74, 222, 128, 0.1)',
                            yAxisID: 'y',
                            tension: 0.4,
                            fill: true
                        },
                        {
                            label: 'CPU Usage (%)',
                            data: [],
                            borderColor: '#60a5fa',
                            backgroundColor: 'rgba(96, 165, 250, 0.1)',
                            yAxisID: 'y1',
                            tension: 0.4,
                            fill: true
                        }
                    ]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    interaction: {
                        mode: 'index',
                        intersect: false,
                    },
                    plugins: {
                        tooltip: {
                            backgroundColor: 'rgba(17, 24, 39, 0.9)',
                            titleFont: { size: 14 },
                            bodyFont: { size: 13 },
                            padding: 12,
                            callbacks: {
                                title: function(ctx) {
                                    return 'UTC Time: ' + ctx[0].label;
                                },
                                afterBody: function(ctx) {
                                    const idx = ctx[0].dataIndex;
                                    const snap = rawHistory[idx];
                                    if(!snap) return '';
                                    let str = '\n--- Details ---\n';
                                    str += 'RAM: ' + (snap.ram / 1024 / 1024).toFixed(2) + ' MB\n';
                                    str += 'Countries:\n';
                                    for(const [c, count] of Object.entries(snap.countries)) {
                                        str += '  ' + c + ': ' + count + '\n';
                                    }
                                    return str;
                                }
                            }
                        },
                        legend: { labels: { color: '#e5e7eb' } }
                    },
                    scales: {
                        x: { ticks: { color: '#9ca3af' }, grid: { color: '#374151' } },
                        y: { 
                            type: 'linear', display: true, position: 'left',
                            ticks: { color: '#4ade80' }, grid: { color: '#374151' },
                            title: { display: true, text: 'Total IPs', color: '#4ade80' }
                        },
                        y1: {
                            type: 'linear', display: true, position: 'right',
                            ticks: { color: '#60a5fa' }, grid: { drawOnChartArea: false },
                            min: 0, max: 100,
                            title: { display: true, text: 'CPU %', color: '#60a5fa' }
                        }
                    }
                }
            });
        }

        async function updateData() {
            try {
                const res = await fetch('/api/stat/data');
                const data = await res.json();

                document.getElementById('uptime').innerText = data.uptime_hours + ' hours ' + data.uptime_mins + ' minutes';
                document.getElementById('totalIps').innerText = data.total_ips;

                rawHistory = data.history;
                const labels = [];
                const ips = [];
                const cpus = [];

                data.history.forEach(h => {
                    const d = new Date(h.time);
                    labels.push(d.toISOString().substr(11, 8));
                    ips.push(h.total_ips);
                    cpus.push(h.cpu.toFixed(2));
                });

                chart.data.labels = labels;
                chart.data.datasets[0].data = ips;
                chart.data.datasets[1].data = cpus;
                chart.update();

                const coresDiv = document.getElementById('coresContainer');
                let coresHtml = '';
                data.cores.forEach((core, i) => {
                    coresHtml += '<div>' +
                        '<div class="flex justify-between text-sm mb-1 text-gray-300">' +
                            '<span>Core ' + i + '</span>' +
                            '<span class="font-mono">' + core.toFixed(1) + '%</span>' +
                        '</div>' +
                        '<div class="w-full bg-gray-700 rounded-full h-2.5">' +
                            '<div class="bg-blue-500 h-2.5 rounded-full transition-all duration-500" style="width: ' + core + '%"></div>' +
                        '</div>' +
                    '</div>';
                });
                coresDiv.innerHTML = coresHtml;

                const countDiv = document.getElementById('countriesContainer');
                let countHtml = '';
                for (const [country, count] of Object.entries(data.countries)) {
                    countHtml += '<div class="flex items-center justify-between bg-gray-700 px-4 py-2 rounded-lg">' +
                        '<span class="font-medium text-gray-200">' + country + '</span>' +
                        '<span class="bg-gray-900 text-green-400 text-xs font-bold px-2 py-1 rounded">' + count + '</span>' +
                    '</div>';
                }
                countDiv.innerHTML = countHtml;

            } catch (e) {}
        }

        initChart();
        updateData();
        setInterval(updateData, 2500);
    </script>
</body>
</html>`

func apiStatUIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(w, bytes.NewBufferString(statHTML))
	}
}
