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

// squareLayout 强制子元素为一个正方形（边长 = min(可用宽, 可用高)），并居中。
// 实现 fyne.Layout 接口。
type squareLayout struct{}

var (
	mutex         sync.Mutex
	questions     []Question
	codePool      []string
	usedCodes     []string
	resultsXlsx   string
	server        *http.Server
	serverRunning bool
	listenAddr    = ":8080"
	baseURL       = ""
	dataDir       = "."
)

func main() {
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
	qrImg1.SetMinSize(fyne.NewSize(260, 260))

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
			codes, err := LoadCodesFromExcel(tmp)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			mutex.Lock()
			exist := map[string]bool{}
			for _, c := range codePool {
				exist[c] = true
			}
			for _, c := range codes {
				if !exist[c] {
					codePool = append(codePool, c)
					exist[c] = true
				}
			}
			mutex.Unlock()
			codeCount.SetText(fmt.Sprintf("兑换码: %d", len(codePool)))
			status.SetText("已加载兑换码")
		}, w)
		//fd.SetTitle("选择兑换码 Excel (.xlsx/.xls)")
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

	// Make QR remain square on resize: listen to window size changes and adjust min size
	w.SetContent(content)
	w.Canvas().SetOnTypedKey(func(ev *fyne.KeyEvent) {}) // noop to ensure canvas exists

	w.ShowAndRun()
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
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		s := a.String()
		if strings.Contains(s, "/") {
			ip := strings.Split(s, "/")[0]
			if strings.HasPrefix(ip, "192.") || strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "172.") {
				return ip
			}
		}
	}
	return "127.0.0.1"
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
		b, err := webFS.ReadFile("web/" + path)
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

	// API: submit (processing implemented previously in storage.SaveResultToExcel usage)
	mux.HandleFunc("/api/submit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string           `json:"name"`
			Phone   string           `json:"phone"`
			IdCard  string           `json:"idcard"`
			Answers map[string][]int `json:"answers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request:"+err.Error(), http.StatusBadRequest)
			return
		}
		// basic validation
		if strings.TrimSpace(req.Name) == "" || len(req.Phone) != 11 {
			http.Error(w, "invalid info", http.StatusBadRequest)
			return
		}

		mutex.Lock()
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
		var assigned string
		if total > 0 && float64(score)/float64(total) >= 0.6 && len(codePool) > 0 {
			assigned = codePool[0]
			codePool = codePool[1:]
			usedCodes = append(usedCodes, assigned)
		}
		mutex.Unlock()

		if resultsXlsx == "" {
			resultsXlsx = filepath.Join(dataDir, "records.xlsx")
		}
		_ = SaveResultToExcel(resultsXlsx, req.Name, req.Phone, req.IdCard, score, total, assigned, detail)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"score": score, "total": total, "code": assigned})
	})
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
