package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bytes"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/xuri/excelize/v2"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/*
var webFS embed.FS

type Question struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Prompt  string   `json:"question"`
	Options []string `json:"options"`
	Answer  []int    `json:"answer"`
	Score   int      `json:"score"`
}

// CheckUserAnswered 检查用户今天是否已经答题
func CheckUserAnswered(path, phoneHash, idHash string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		return false, err
	}

	rows, err := f.GetRows("Sheet1")
	if err != nil {
		return false, err
	}

	today := time.Now().Format("2006-01-02")

	for i, r := range rows {
		if i == 0 {
			continue // skip header
		}
		if len(r) >= 8 { // 确保有足够的列，第9列是user_hash
			timestamp := strings.TrimSpace(r[0])
			recordPhoneHash := strings.TrimSpace(r[2]) // 第9列是id_hash
			recordIdHash := strings.TrimSpace(r[3])    // 第10列是phone_hash

			// 检查是否是今天的记录并且哈希匹配
			if strings.Contains(timestamp, today) && recordPhoneHash == phoneHash || strings.Contains(timestamp, today) && recordIdHash == idHash {
				return true, nil
			}
		}
	}

	return false, nil
}

// squareLayout 强制子元素为一个正方形（边长 = min(可用宽, 可用高)），并居中。
// 实现 fyne.Layout 接口。
type squareLayout struct{}

var (
	mutex         sync.Mutex
	questions     []Question
	prizeLevels   []PrizeLevel
	prizeCodes    []PrizeCode
	usedCodes     []string
	resultsXlsx   string
	server        *http.Server
	serverRunning bool
	listenAddr    = ":8080"
	baseURL       = ""
	dataDir       = "."
)

func main() {
	os.Setenv("FYNE_SCALE", "1")

	a := app.NewWithID("com.example.quizmanager")
	a.Settings().SetTheme(theme.DarkTheme())
	w := a.NewWindow("反诈答题 管理后台")
	w.Resize(fyne.NewSize(1000, 640))

	// Left controls
	status := widget.NewLabel("就绪")
	qCount := widget.NewLabel("题目: 0")
	codeCount := widget.NewLabel("兑换码: 0")

	// QR image (square) - canvas Image
	qrImg1 := canvas.NewImageFromImage(nil)
	qrImg1.FillMode = canvas.ImageFillContain
	// ensure min size square
	qrImg1.SetMinSize(fyne.NewSize(200, 200))

	// Buttons
	btnLoadQ := widget.NewButton("加载题库 Excel", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if rc == nil {
				return
			}
			// Read URI (works on Android and Desktop)
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			tmp := filepath.Join(a.Storage().RootURI().Path(), "import_questions.xlsx")
			if err := os.WriteFile(tmp, data, 0644); err != nil {
				dialog.ShowError(err, w)
				return
			}
			qs, err := LoadQuestionsFromExcel(tmp)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			mutex.Lock()
			questions = qs
			mutex.Unlock()
			qCount.SetText(fmt.Sprintf("题目: %d", len(qs)))
			status.SetText("已加载题库")
		}, w)
		//fd.SetTitle("选择题库 Excel (.xlsx/.xls)")
		fd.Show()
	})

	btnLoadC := widget.NewButton("加载兑换码 Excel", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if rc == nil {
				return
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			tmp := filepath.Join(a.Storage().RootURI().Path(), "import_codes.xlsx")
			if err := os.WriteFile(tmp, data, 0644); err != nil {
				dialog.ShowError(err, w)
				return
			}
			levels, codes, err := LoadCodesFromExcel(tmp)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}

			// Filter out codes that have been used today
			availableCodes := []PrizeCode{}
			for _, code := range codes {
				used, err := IsCodeUsedToday(resultsXlsx, code.Code)
				if err != nil {
					log.Printf("检查兑换码使用状态错误: %v", err)
					continue
				}
				if !used {
					availableCodes = append(availableCodes, code)
				}
			}

			mutex.Lock()
			prizeLevels = levels
			prizeCodes = availableCodes
			mutex.Unlock()

			codeCount.SetText(fmt.Sprintf("可用兑换码: %d", len(availableCodes)))
			status.SetText(fmt.Sprintf("已加载 %d 个奖品等级, %d 个可用兑换码", len(levels), len(availableCodes)))
		}, w)
		fd.Show()
	})

	btnLoadPath := widget.NewButton("加载结果保存路径 Excel", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if rc == nil {
				return
			}
			// read and save as temp to parse
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			tmp := filepath.Join(a.Storage().RootURI().Path(), "import_result_path.xlsx")
			if err := os.WriteFile(tmp, data, 0644); err != nil {
				dialog.ShowError(err, w)
				return
			}
			p, err := LoadResultPathFromExcel(tmp)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			resultsXlsx = p
			status.SetText("结果路径已设置: " + p)
		}, w)
		//fd.SetTitle("选择结果保存路径 Excel")
		fd.Show()
	})

	var btnToggle *widget.Button
	btnToggle = widget.NewButton("启动 Web 服务", func() {
		if serverRunning {
			if server != nil {
				_ = server.Close()
			}
			serverRunning = false
			btnToggle.SetText("启动 Web 服务")
			status.SetText("服务已停止")
		} else {
			mux := http.NewServeMux()
			// serve templates from embed and static via /static/
			setupWebHandlers(mux)
			server = &http.Server{
				Addr:    listenAddr,
				Handler: mux,
			}
			go func() {
				serverRunning = true
				btnToggle.SetText("停止 Web 服务")
				baseURL = "http://" + localIP() + listenAddr
				status.SetText("服务运行中，访问: " + baseURL)
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Println("server error:", err)
				}
				serverRunning = false
				btnToggle.SetText("启动 Web 服务")
				status.SetText("服务已停止")
			}()
		}
	})

	btnQR := widget.NewButton("生成二维码并显示", func() {
		u := baseURL
		if u == "" {
			u = "http://" + localIP() + listenAddr
		}
		pngBytes, err := generateQRCodeBytes(u + "/identity.html")
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		// decode to image.Image to avoid resource resizing artifacts
		img, err := png.Decode(bytes.NewReader(pngBytes))
		if err == nil {
			qrImg1.Image = img
		} else {
			res := fyne.NewStaticResource("qr.png", pngBytes)
			qrImg1.Resource = res
		}
		qrImg1.Refresh()
		status.SetText("二维码生成，访问: " + u + "/identity.html")
	})

	btnExport := widget.NewButton("显示结果文件路径", func() {
		p := resultsXlsx
		if p == "" {
			p = filepath.Join(dataDir, "records.xlsx")
		}
		dialog.ShowInformation("结果文件路径", p, w)
	})

	// Layout: left sidebar (controls), right content (QR + status) responsive
	left := container.NewVBox(
		widget.NewLabelWithStyle("反诈答题 - 管理后台", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		status, container.NewHBox(qCount, codeCount),
		layout.NewSpacer(),
		btnLoadQ, btnLoadC, btnLoadPath, btnToggle, btnQR, btnExport,
	)

	// 确保 qrImg 的 FillMode 为 ImageFillContain，保证图片按比例缩放
	qrImg1.FillMode = canvas.ImageFillContain

	// 使用自定义 layout 保证正方形
	qrSquare := container.New(&squareLayout{}, qrImg1)

	// 在右侧放置 QR 与文本信息
	rightInfoLabel := widget.NewLabel("访问链接 (手机扫码或点击):")
	rightLinkLabel := widget.NewLabelWithStyle("http://<ip>:8080/identity.html", fyne.TextAlignLeading, fyne.TextStyle{})
	right := container.NewVBox(
		qrSquare,
		container.NewVBox(rightInfoLabel, rightLinkLabel),
	)

	// Build main container that adapts to window size: left fixed, right expands
	content := container.New(layout.NewBorderLayout(left, nil, nil, nil), left, right)
	// 👉 加滚动容器（关键）
	scroll := container.NewVScroll(content)

	w.SetContent(scroll)
	// Make QR remain square on resize: listen to window size changes and adjust min size
	//w.SetContent(content)
	w.Canvas().SetOnTypedKey(func(ev *fyne.KeyEvent) {}) // noop to ensure canvas exists

	w.ShowAndRun()
	fmt.Println("DPI Scale =", w.Canvas().Scale())

}

// resizeListener implements Fyne canvas listener to adjust QR size (simple approach)
type resizeListener struct {
	qr  *canvas.Image
	win fyne.Window
}

func (r *resizeListener) TypedKey(*fyne.KeyEvent)      {}
func (r *resizeListener) TypedRune(rn rune)            {}
func (r *resizeListener) MouseMoved(p fyne.Position)   {}
func (r *resizeListener) MouseDown(p *fyne.PointEvent) {}
func (r *resizeListener) MouseUp(p *fyne.PointEvent)   {}
func (r *resizeListener) Scrolled(*fyne.ScrollEvent)   {}
func (r *resizeListener) FocusGained()                 {}
func (r *resizeListener) FocusLost()                   {}
func (r *resizeListener) Detached()                    {}
func (r *resizeListener) CanvasResized(size fyne.Size) {
	// Choose 40% of smaller dimension for QR to keep square
	w := r.win.Canvas().Size()
	min := w.Width
	if w.Height < min {
		min = w.Height
	}
	target := int(0.4 * float32(min))
	if target < 160 {
		target = 160
	}
	r.qr.SetMinSize(fyne.NewSize(float32(target), float32(target)))
	r.qr.Refresh()
}

func localIP() string {
	return getLocalIP()
}

// getLocalIP 获取本地IP地址（兼容Android 10+）
func getLocalIP() string {
	// 方法1: 通过连接外部DNS服务器获取本机IP
	if ip := getIPFromDNS(); ip != "" {
		return ip
	}
	// 方法2: 尝试常见的局域网网卡
	if ip := getIPFromInterfaces(); ip != "" {
		return ip
	}

	// 方法3: 使用net.LookupHost获取主机名对应的IP
	if ip := getIPFromHostname(); ip != "" {
		return ip
	}

	return "127.0.0.1"
}

// getIPFromDNS 通过连接DNS服务器获取本机IP
func getIPFromDNS() string {
	// 尝试多个公共DNS服务器
	dnsServers := []string{
		"8.8.8.8:53",         // Google DNS
		"1.1.1.1:53",         // Cloudflare DNS
		"208.67.222.222:53",  // OpenDNS
		"114.114.114.114:53", // 114 DNS
	}
	for _, dnsServer := range dnsServers {
		conn, err := net.Dial("udp", dnsServer)
		if err != nil {
			continue
		}

		localAddr := conn.LocalAddr().(*net.UDPAddr)
		ip := localAddr.IP.String()
		conn.Close()

		// 检查是否是有效的局域网IP
		if isValidLocalIP(ip) {
			return ip
		}
	}
	return ""
}

// getIPFromInterfaces 从网络接口获取IP
func getIPFromInterfaces() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		// 跳过回环接口和未启用接口
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue
			}

			ipStr := ip.String()
			if isValidLocalIP(ipStr) {
				return ipStr
			}
		}
	}
	return ""
}

// getIPFromHostname 通过主机名获取IP
func getIPFromHostname() string {
	addrs, err := net.LookupHost("localhost")
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
			ipStr := ip.String()
			if isValidLocalIP(ipStr) {
				return ipStr
			}
		}
	}
	return ""
}

// isValidLocalIP 检查是否是有效的局域网IP
func isValidLocalIP(ip string) bool {
	if ip == "127.0.0.1" || ip == "::1" || ip == "0.0.0.0" {
		return false
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	// 检查私有IP地址范围
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // 链路本地地址
	}

	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(parsedIP) {
			return true
		}
	}

	return false
}

// getNetworkInfo 获取网络信息（主函数）
func getNetworkInfo() string {
	ip := getLocalIP()
	// 如果获取不到有效IP，提供使用说明
	if ip == "127.0.0.1" {
		return "无法自动获取IP，请手动查看手机IP地址"
	}

	return ip
}

// setupWebHandlers mounts template endpoints and static resource handler (serves from embed)
func setupWebHandlers(mux *http.ServeMux) {
	// templates
	tStart := template.Must(template.ParseFS(webFS, "web/start.html"))
	tIdentity := template.Must(template.ParseFS(webFS, "web/identity.html"))
	tQuiz := template.Must(template.ParseFS(webFS, "web/quiz.html"))
	tReward := template.Must(template.ParseFS(webFS, "web/reward.html"))

	// root -> start page
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tStart.Execute(w, nil)
	})
	// API: 获取网络信息
	mux.HandleFunc("/api/network-info", func(w http.ResponseWriter, r *http.Request) {
		ip := getLocalIP()
		info := map[string]string{
			"ip":     ip,
			"url":    "http://" + ip + listenAddr,
			"status": "ready",
		}

		if ip == "127.0.0.1" {
			info["message"] = "无法自动获取IP，请手动查看手机IP地址"
			info["status"] = "manual_required"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})

	// API: check-user 检查用户是否已经答题
	mux.HandleFunc("/api/check-user", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			PhoneHash string `json:"phone_hash"`
			IdHash    string `json:"id_hash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request:"+err.Error(), http.StatusBadRequest)
			return
		}

		if req.PhoneHash == "" || req.IdHash == "" {
			http.Error(w, "user_hash is required", http.StatusBadRequest)
			return
		}

		mutex.Lock()
		defer mutex.Unlock()

		answered, err := CheckUserAnswered(resultsXlsx, req.PhoneHash, req.IdHash)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{
			"answered": answered,
		})
	})

	mux.HandleFunc("/identity.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tIdentity.Execute(w, nil)
	})
	mux.HandleFunc("/quiz.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tQuiz.Execute(w, nil)
	})
	mux.HandleFunc("/reward.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tReward.Execute(w, nil)
	})

	// static resources served from embed at /static/
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		// strip /static/
		path := strings.TrimPrefix(r.URL.Path, "/static/")
		if path == "" {
			http.NotFound(w, r)
			return
		}
		// read from embed
		b, err := webFS.ReadFile("web/static/" + path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		case ".png":
			w.Header().Set("Content-Type", "image/png")
		case ".jpg", ".jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
		case ".html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(b)
	})
	// API: start-info
	mux.HandleFunc("/api/start-info", func(w http.ResponseWriter, r *http.Request) {
		u := baseURL
		if u == "" {
			u = "http://" + localIP() + listenAddr
		}

		qb64, _ := generateQRCodeBase64(u + "/identity.html")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"exam_url": u + "/identity.html",
			"qrcode":   "data:image/png;base64," + qb64,
		})
	})
	// API: questions (returns shuffled)
	mux.HandleFunc("/api/questions", func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		arr := make([]Question, len(questions))
		copy(arr, questions)
		mutex.Unlock()
		// shuffle
		for i := range arr {
			j := int(time.Now().UnixNano()) % (len(arr) + 1)
			if j >= len(arr) {
				j = 0
			}
			arr[i], arr[j] = arr[j], arr[i]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(arr)
	})

	// API: submit (新的奖品发放逻辑)
	mux.HandleFunc("/api/submit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string           `json:"name"`
			Phone      string           `json:"phone"`
			IdCard     string           `json:"idCard"`
			MaskName   string           `json:"mask_name"`
			MaskPhone  string           `json:"mask_phone"`
			MaskIdCard string           `json:"mask_idCard"`
			Answers    map[string][]int `json:"answers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request:"+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Name) == "" || len(req.Phone) != 64 || len(req.IdCard) != 64 {
			http.Error(w, "invalid info", http.StatusBadRequest)
			return
		}

		mutex.Lock()
		defer mutex.Unlock()

		total := 0
		score := 0
		detail := map[string]interface{}{}
		for _, q := range questions {
			s := 1
			if q.Score > 0 {
				s = q.Score
			}
			total += s
			given := req.Answers[q.ID]
			gotScore, ok := gradeQuestion(q, given)
			if ok {
				score += gotScore
			}
			givenLabels := []string{}
			for _, gi := range given {
				if gi >= 0 && gi < len(q.Options) {
					givenLabels = append(givenLabels, q.Options[gi])
				}
			}
			detail[q.ID] = map[string]interface{}{"given": givenLabels, "correct": gotScore > 0}
		}

		// 新的奖品发放逻辑
		var assigned string
		var prizeLevel string

		if total > 0 && len(prizeCodes) > 0 {
			// 计算百分比分数
			percentage := int(float64(score) / float64(total) * 100)

			// 按奖品等级从高到低尝试分配
			for _, levelConfig := range prizeLevels {
				if percentage >= levelConfig.Score {
					// 尝试分配该等级的奖品
					assigned, prizeLevel = assignPrizeByLevel(levelConfig.Level)
					if assigned != "" {
						break // 成功分配到奖品，退出循环
					}
				}
			}
		}

		if assigned != "" {
			usedCodes = append(usedCodes, assigned)
		}

		if resultsXlsx == "" {
			resultsXlsx = filepath.Join(dataDir, "records.xlsx")
		}
		_ = SaveResultToExcel(resultsXlsx, req.Name, req.Phone, req.IdCard, req.MaskName, req.MaskPhone, req.MaskIdCard, score, total, assigned, detail)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"score":       score,
			"total":       total,
			"percentage":  int(float64(score) / float64(total) * 100),
			"code":        assigned,
			"prize_level": prizeLevel,
		})
	})
}

// assignPrizeByLevel 按等级分配奖品
func assignPrizeByLevel(level string) (string, string) {
	for i, prize := range prizeCodes {
		if prize.Level == level && !prize.Used {
			// 标记为已使用并从可用列表中移除
			prizeCodes[i].Used = true
			return prize.Code, level
		}
	}
	return "", ""
}

func (s *squareLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	if len(objects) == 0 {
		return
	}
	obj := objects[0]
	// 计算边长 = min(width, height)
	side := size.Width
	if size.Height < side {
		side = size.Height
	}
	// 需要转换为 float32
	sideF := float32(side)
	// 计算左上角坐标以居中
	x := (size.Width - sideF) / 2
	y := (size.Height - sideF) / 2

	obj.Move(fyne.NewPos(x, y))
	obj.Resize(fyne.NewSize(sideF, sideF))
}

func (s *squareLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	if len(objects) == 0 {
		return fyne.NewSize(0, 0)
	}
	// 如果子元素有最小尺寸，返回其最小边的正方形尺寸；否则返回默认
	min := objects[0].MinSize()
	side := min.Width
	if min.Height < side {
		side = min.Height
	}
	return fyne.NewSize(side, side)
}

//func generateQRCodeBase64(url string) (string, error) {
//	pngBytes, err := qrcode.Encode(url, qrcode.Medium, 512)
//	if err != nil {
//		return "", err
//	}
//	return base64.StdEncoding.EncodeToString(pngBytes), nil
//}

func generateQRCodeBytes(url string) ([]byte, error) {
	return qrcode.Encode(url, qrcode.Medium, 512)
}

func gradeQuestion(q Question, given []int) (int, bool) {
	if q.Type == "single" || q.Type == "judge" {
		if len(q.Answer) > 0 && len(given) == 1 && q.Answer[0] == given[0] {
			if q.Score > 0 {
				return q.Score, true
			}
			return 1, true
		}
		return 0, false
	}
	if len(q.Answer) != len(given) {
		return 0, false
	}
	m := map[int]bool{}
	for _, a := range q.Answer {
		m[a] = true
	}
	for _, g := range given {
		if !m[g] {
			return 0, false
		}
	}
	if q.Score > 0 {
		return q.Score, true
	}
	return 1, true
}
