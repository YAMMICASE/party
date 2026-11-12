package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static
var staticFiles embed.FS

const (
	PlayerRadius = 17.0  // радиус "ног" для коллизий (увеличен под масштаб 1.4)
	Speed        = 230.0 // пикселей в секунду
	TickRate     = 30    // обновлений в секунду
	TopMargin    = 90.0  // отступ сверху с учётом увеличенной головы
)

// ---------- Локация ----------

type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Circle struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	R float64 `json:"r"`
}

type Screen struct {
	Rect
	On       bool   `json:"on"`
	VideoURL string `json:"videoUrl"`
}

type Layout struct {
	W          float64 `json:"w"`
	H          float64 `json:"h"`
	Stage      Rect    `json:"stage"`
	Screen     Screen  `json:"screen"`
	Tree       Circle  `json:"tree"`
	Table      Rect    `json:"table"`
	DanceFloor Rect    `json:"danceFloor"`
}

func defaultLayout() Layout {
	// Предметы увеличены в 1.4 раза, но ширина сцены, экрана и танцпола сохранена
	return Layout{
		W:          1600,
		H:          1000,
		Stage:      Rect{X: 480, Y: 100, W: 640, H: 308},
		Screen:     Screen{Rect: Rect{X: 580, Y: 15, W: 440, H: 210}, On: false, VideoURL: ""},
		Tree:       Circle{X: 1360, Y: 360, R: 84},
		Table:      Rect{X: 50, Y: 400, W: 380, H: 150},
		DanceFloor: Rect{X: 560, Y: 460, W: 480, H: 480},
	}
}

// ---------- Игроки ----------

type Input struct {
	Up    bool `json:"up"`
	Down  bool `json:"down"`
	Left  bool `json:"left"`
	Right bool `json:"right"`
}

type Player struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Color     string  `json:"color"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Dir       int     `json:"dir"`
	Moving    bool    `json:"moving"`
	Action    string  `json:"action"`
	ActionEnd int64   `json:"-"`
	input     Input
}

var actionDur = map[string]time.Duration{
	"dance":     10 * time.Second,
	"sparkler":  7 * time.Second,
	"champagne": 4 * time.Second,
}

var palette = []string{
	"#ff8fab", "#ffd166", "#8ecae6", "#b5e48c", "#cdb4db",
	"#f4a261", "#90e0ef", "#ffadad", "#caffbf", "#bdb2ff",
}

// ---------- Сообщения ----------

type InMsg struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Action string `json:"action"`
	Input
}

type WelcomeMsg struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Layout Layout `json:"layout"`
}

type StateMsg struct {
	Type    string    `json:"type"`
	Players []*Player `json:"players"`
}

// ---------- Игра ----------

type Client struct {
	conn   *websocket.Conn
	send   chan []byte
	player *Player
}

type Game struct {
	mu      sync.Mutex
	clients map[*Client]bool
	layout  Layout
	rects   []Rect
	guestN  int
}

func NewGame() *Game {
	l := defaultLayout()
	g := &Game{clients: map[*Client]bool{}, layout: l}
	g.rects = []Rect{
		l.Table,
		{X: l.Screen.X, Y: l.Screen.Y, W: l.Screen.W, H: l.Screen.H + 28},
	}
	return g
}

func circleRect(cx, cy, r float64, o Rect) bool {
	nx := math.Max(o.X, math.Min(cx, o.X+o.W))
	ny := math.Max(o.Y, math.Min(cy, o.Y+o.H))
	return math.Hypot(cx-nx, cy-ny) < r
}

func (g *Game) free(x, y float64) bool {
	r := PlayerRadius
	l := g.layout
	if x < r || x > l.W-r || y < TopMargin || y > l.H-r {
		return false
	}
	for _, o := range g.rects {
		if circleRect(x, y, r, o) {
			return false
		}
	}
	if math.Hypot(x-l.Tree.X, y-l.Tree.Y) < l.Tree.R+r {
		return false
	}
	return true
}

func (g *Game) spawnPoint() (float64, float64) {
	for i := 0; i < 200; i++ {
		x := 680 + rand.Float64()*240
		y := 420 + rand.Float64()*100
		if g.free(x, y) {
			return x, y
		}
	}
	return 800, 480
}

func (g *Game) step(p *Player, dt float64, nowMs int64) {
	dx, dy := 0.0, 0.0
	if p.input.Left {
		dx--
	}
	if p.input.Right {
		dx++
	}
	if p.input.Up {
		dy--
	}
	if p.input.Down {
		dy++
	}
	if dx != 0 || dy != 0 {
		l := math.Hypot(dx, dy)
		dx, dy = dx/l, dy/l
		nx := p.X + dx*Speed*dt
		ny := p.Y + dy*Speed*dt
		if g.free(nx, p.Y) {
			p.X = nx
		}
		if g.free(p.X, ny) {
			p.Y = ny
		}
		if dx < 0 {
			p.Dir = -1
		} else if dx > 0 {
			p.Dir = 1
		}
		p.Moving = true
		if p.Action == "dance" {
			p.Action = ""
		}
	} else {
		p.Moving = false
	}
	if p.Action != "" && nowMs > p.ActionEnd {
		p.Action = ""
	}
}

func (g *Game) run() {
	ticker := time.NewTicker(time.Second / TickRate)
	last := time.Now()
	for now := range ticker.C {
		dt := now.Sub(last).Seconds()
		last = now
		nowMs := now.UnixMilli()

		g.mu.Lock()
		players := make([]*Player, 0, len(g.clients))
		for c := range g.clients {
			if c.player == nil {
				continue
			}
			g.step(c.player, dt, nowMs)
			players = append(players, c.player)
		}
		msg, _ := json.Marshal(StateMsg{Type: "state", Players: players})
		for c := range g.clients {
			select {
			case c.send <- msg:
			default:
			}
		}
		g.mu.Unlock()
	}
}

// ---------- WebSocket ----------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (g *Game) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Client{conn: conn, send: make(chan []byte, 64)}
	go c.writePump()
	g.readPump(c)
}

func (g *Game) readPump(c *Client) {
	defer func() {
		g.mu.Lock()
		if g.clients[c] {
			delete(g.clients, c)
		}
		close(c.send)
		g.mu.Unlock()
		c.conn.Close()
	}()

	c.conn.SetReadLimit(2048)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var m InMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}

		g.mu.Lock()
		switch m.Type {
		case "join":
			if c.player == nil {
				name := strings.TrimSpace(m.Name)
				if rs := []rune(name); len(rs) > 16 {
					name = string(rs[:16])
				}
				if name == "" {
					g.guestN++
					name = fmt.Sprintf("Гость %d", g.guestN)
				}
				x, y := g.spawnPoint()
				c.player = &Player{
					ID:    fmt.Sprintf("%x", rand.Int63()),
					Name:  name,
					Color: palette[rand.Intn(len(palette))],
					X:     x, Y: y, Dir: 1,
				}
				g.clients[c] = true
				w, _ := json.Marshal(WelcomeMsg{Type: "welcome", ID: c.player.ID, Layout: g.layout})
				c.send <- w
			}
		case "input":
			if c.player != nil {
				c.player.input = m.Input
			}
		case "action":
			if c.player != nil {
				if d, ok := actionDur[m.Action]; ok {
					if c.player.Action == m.Action {
						c.player.Action = ""
					} else {
						c.player.Action = m.Action
						c.player.ActionEnd = time.Now().Add(d).UnixMilli()
					}
				}
			}
		}
		g.mu.Unlock()
	}
}

func (c *Client) writePump() {
	ping := time.NewTicker(30 * time.Second)
	defer func() {
		ping.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func main() {
	game := NewGame()
	go game.run()

	sub, _ := fs.Sub(staticFiles, "static")
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/ws", game.handleWS)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Корпоратив запущен: http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
