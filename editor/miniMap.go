package editor

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/akiyosi/gonvim/util"
	"github.com/neovim/go-client/nvim"
	"github.com/therecipe/qt/core"
	"github.com/therecipe/qt/gui"
	"github.com/therecipe/qt/widgets"
)

type miniMapSignal struct {
	core.QObject
	_ func() `signal:"stopSignal"`
	_ func() `signal:"redrawSignal"`
	_ func() `signal:"guiSignal"`
}

// MiniMap is
type MiniMap struct {
	ws        *Workspace
	widget    *widgets.QWidget
	curRegion *widgets.QWidget
	pos       int
	width     int
	height    int
	currBuf   string

	visible bool

	font             *Font
	content          [][]*Cell
	scrollRegion     []int
	scrollDust       [2]int
	scrollDustDeltaY int
	queueRedrawArea  [4]int
	paintMutex       sync.Mutex
	redrawMutex      sync.Mutex
	bg               *RGBA
	curtab           nvim.Tabpage
	cursor           [2]int
	cmdheight        int
	curWins          map[nvim.Window]*Window
	highlight        Highlight
	isSetColorscheme bool

	sync          sync.Mutex
	signal        *miniMapSignal
	redrawUpdates chan [][]interface{}
	guiUpdates    chan []interface{}
	stopOnce      sync.Once
	stop          chan struct{}

	nvim       *nvim.Nvim
	uiAttached bool
	rows       int
	cols       int

	foreground *RGBA
	background *RGBA
	special    *RGBA
}

func newMiniMap() *MiniMap {
	widget := widgets.NewQWidget(nil, 0)
	widget.SetContentsMargins(0, 0, 0, 0)
	widget.SetAttribute(core.Qt__WA_OpaquePaintEvent, true)
	widget.SetStyleSheet(" * { background-color: rgba(0, 0, 0, 0);}")
	widget.SetFixedWidth(140)

	curRegion := widgets.NewQWidget(widget, 0)
	curRegion.SetAttribute(core.Qt__WA_OpaquePaintEvent, true)
	curRegion.SetStyleSheet(" * { background-color: rgba(255, 255, 255, 30);}")
	curRegion.SetFixedWidth(140)
	curRegion.SetFixedHeight(1)

	m := &MiniMap{
		widget:        widget,
		curRegion:     curRegion,
		scrollRegion:  []int{0, 0, 0, 0},
		stop:          make(chan struct{}),
		signal:        NewMiniMapSignal(nil),
		redrawUpdates: make(chan [][]interface{}, 1000),
		guiUpdates:    make(chan []interface{}, 1000),
	}
	m.signal.ConnectRedrawSignal(func() {
		updates := <-m.redrawUpdates
		m.handleRedraw(updates)
	})
	m.signal.ConnectGuiSignal(func() {
		updates := <-m.guiUpdates
		m.handleRPCGui(updates)
	})
	m.signal.ConnectStopSignal(func() {
	})
	m.widget.ConnectPaintEvent(m.paint)
	m.widget.ConnectResizeEvent(func(event *gui.QResizeEvent) {
		m.updateSize()
	})
	m.widget.ConnectMousePressEvent(m.mouseEvent)
	m.widget.ConnectWheelEvent(m.wheelEvent)
	m.widget.Hide()

	switch runtime.GOOS {
	case "windows":
		m.font = initFontNew("Consolas", 1, 0)
	case "darwin":
		m.font = initFontNew("Courier New", 2, 0)
	default:
		m.font = initFontNew("Monospace", 1, 0)
	}

	return m
}

func (m *MiniMap) startMinimapProc() {
	neovim, err := nvim.NewChildProcess(nvim.ChildProcessArgs("-u", "NONE", "-n", "--embed", "--headless"))
	if err != nil {
		fmt.Println(err)
	}
	m.nvim = neovim
	m.nvim.RegisterHandler("Gui", func(updates ...interface{}) {
		m.guiUpdates <- updates
		m.signal.GuiSignal()
	})
	m.nvim.RegisterHandler("redraw", func(updates ...[]interface{}) {
		m.redrawUpdates <- updates
		m.signal.RedrawSignal()
	})
	m.width = m.widget.Width()
	m.height = m.widget.Height()

	m.updateSize()

	go func() {
		err = m.nvim.Serve()
		if err != nil {
			fmt.Println(err)
		}
		m.stopOnce.Do(func() {
			close(m.stop)
		})
		m.signal.StopSignal()
	}()

	err = m.nvim.AttachUI(m.cols, m.rows, m.attachUIOption())
	if err != nil {
		fmt.Println(err)
	}
	m.uiAttached = true
	m.visible = editor.config.MiniMap.Visible

	m.nvim.Subscribe("Gui")
	m.nvim.Command(":set laststatus=0 | set noruler")
	m.nvim.Command(":syntax on")
	m.nvim.Command(":set nowrap")
}

func (m *MiniMap) exit() {
	go m.nvim.Command(":q!")
}

func (m *MiniMap) attachUIOption() map[string]interface{} {
	o := make(map[string]interface{})
	o["rgb"] = true

	apiInfo, err := m.nvim.APIInfo()
	if err == nil {
		for _, item := range apiInfo {
			i, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			for k, v := range i {
				if k != "ui_events" {
					continue
				}
				events, ok := v.([]interface{})
				if !ok {
					continue
				}
				for _, event := range events {
					function, ok := event.(map[string]interface{})
					if !ok {
						continue
					}
					_, ok = function["name"]
					if !ok {
						continue
					}
					//if name == "wildmenu_show" {
					//	o["ext_wildmenu"] = true
					//} else if name == "cmdline_show" {
					//	o["ext_cmdline"] = true
					//} else if name == "msg_chunk" {
					//	o["ext_messages"] = true
					//} else if name == "popupmenu_show" {
					//	o["ext_popupmenu"] = true
					//} else if name == "tabline_update" {
					//	o["ext_tabline"] = m.drawTabline
					//}
				}
			}
		}
	}
	return o
}

func (m *MiniMap) setColor() {
	c := editor.colors.selectedBg
	m.curRegion.SetStyleSheet(fmt.Sprintf(" * { background-color: rgba(%d, %d, %d, 0.3);}", c.R, c.G, c.B))
}

func (m *MiniMap) toggle() {
	if m.visible {
		m.visible = false
	} else {
		m.visible = true
	}
	m.bufUpdate()
}

func (m *MiniMap) paint(vqp *gui.QPaintEvent) {
	m.paintMutex.Lock()
	defer m.paintMutex.Unlock()

	rect := vqp.M_rect()
	font := m.font
	top := rect.Y()
	left := rect.X()
	width := rect.Width()
	height := rect.Height()
	right := left + width
	bottom := top + height
	row := int(float64(top) / float64(font.lineHeight))
	col := int(float64(left) / font.truewidth)
	rows := int(math.Ceil(float64(bottom)/float64(font.lineHeight))) - row
	cols := int(math.Ceil(float64(right)/font.truewidth)) - col

	p := gui.NewQPainter2(m.widget)
	p.SetBackgroundMode(core.Qt__TransparentMode)
	bg := m.ws.background
	transparent := int(math.Trunc(editor.config.Editor.Transparent * float64(255)))
	if transparent < 255 {
		transparent = 0
	}
	if m.background != nil {
		// p.FillRect5(
		p.FillRect2(
			left,
			top,
			width,
			height,
			// m.background.QColor(),
			gui.NewQBrush3(gui.NewQColor3(bg.R, bg.G, bg.B, transparent), core.Qt__SolidPattern),
		)
	}

	p.SetFont(font.fontNew)

	for y := row; y < row+rows; y++ {
		if y >= m.rows {
			continue
		}
		m.fillHightlight(p, y, col, cols, [2]int{0, 0})
		m.drawText(p, y, col, cols, [2]int{0, 0})
	}

	// m.drawWindows(p, row, col, rows, cols)
	p.DestroyQPainter()
}

// func (m *MiniMap) drawWindows(p *gui.QPainter, row, col, rows, cols int) {
// 	done := make(chan struct{}, 1000)
// 	go func() {
// 		m.getWindows()
// 		close(done)
// 	}()
// 	select {
// 	case <-done:
// 	case <-time.After(1 * time.Millisecond):
// 	}
// }
//
// func (m *MiniMap) getWindows() {
// 	wins := map[nvim.Window]*Window{}
// 	neovim := m.nvim
// 	curtab, _ := neovim.CurrentTabpage()
// 	m.curtab = curtab
// 	nwins, _ := neovim.TabpageWindows(curtab)
// 	b := neovim.NewBatch()
// 	for _, nwin := range nwins {
// 		win := &Window{
// 			win: nwin,
// 		}
// 		b.WindowWidth(nwin, &win.width)
// 		b.WindowHeight(nwin, &win.height)
// 		b.WindowPosition(nwin, &win.pos)
// 		b.WindowTabpage(nwin, &win.tab)
// 		wins[nwin] = win
// 	}
// 	b.Option("cmdheight", &m.cmdheight)
// 	err := b.Execute()
// 	if err != nil {
// 		return
// 	}
// 	m.curWins = wins
// 	for _, win := range m.curWins {
// 		buf, _ := neovim.WindowBuffer(win.win)
// 		win.bufName, _ = neovim.BufferName(buf)
//
// 		if win.height+win.pos[0] < m.rows-m.cmdheight {
// 			win.statusline = true
// 		} else {
// 			win.statusline = false
// 		}
// 		neovim.WindowOption(win.win, "winhl", &win.hl)
// 		if win.hl != "" {
// 			parts := strings.Split(win.hl, ",")
// 			for _, part := range parts {
// 				if strings.HasPrefix(part, "Normal:") {
// 					hl := part[7:]
// 					result := ""
// 					neovim.Eval(fmt.Sprintf("synIDattr(hlID('%s'), 'bg')", hl), &result)
// 					if result != "" {
// 						var r, g, b int
// 						format := "#%02x%02x%02x"
// 						n, err := fmt.Sscanf(result, format, &r, &g, &b)
// 						if err != nil {
// 							continue
// 						}
// 						if n != 3 {
// 							continue
// 						}
// 						win.bg = newRGBA(r, g, b, 1)
// 					}
// 				}
// 			}
// 		}
// 	}
// }

func (m *MiniMap) updateRows() bool {
	var ret bool
	m.height = m.widget.Height()
	rows := m.height / m.font.lineHeight

	if rows != m.rows {
		ret = true
	}
	m.rows = rows
	return ret
}

func (m *MiniMap) updateCols() bool {
	var ret bool
	m.width = m.widget.Width()
	cols := int(float64(m.width) / m.font.truewidth)

	if cols != m.cols {
		ret = true
	}
	m.cols = cols
	return ret
}

func (m *MiniMap) updateSize() {
	isColDiff := m.updateCols()
	isRowDiff := m.updateRows()
	isTryResize := isColDiff || isRowDiff
	if m.uiAttached && isTryResize {
		m.nvim.TryResizeUI(m.cols, m.rows)
	}
}

func (m *MiniMap) bufUpdate() {
	if strings.Contains(m.ws.filepath, "[denite]") {
		return
	}
	if !m.visible {
		m.widget.Hide()
		return
	}
	if m.ws.nvim == nil || m.nvim == nil {
		return
	}
	if !m.isSetColorscheme {
		m.setColorscheme()
	}
	m.widget.Show()

	if m.currBuf == m.ws.filepath {
		return
	}
	m.currBuf = m.ws.filepath

	if m.currBuf == "" {
		go m.nvim.Command(":e! [No Name]")
	} else {
		go m.nvim.Command(":e! " + m.currBuf)
	}
	m.mapScroll()
}

func (m *MiniMap) setColorscheme() {
	basepath, _ := m.ws.nvim.CommandOutput("echo g:dein#_base_path")
	packpath := basepath + `/repos/github.com/`
	colo, _ := m.ws.nvim.CommandOutput("colo")
	lsDirs, _ := ioutil.ReadDir(packpath)

	// Set coloDir and
	//  some exceptional color scheme names and plugin directory names
	coloDir := ""
	switch colo {
	case "one":
		coloDir = "vim-one"
	case "primery":
		coloDir = "vim-colorscheme-primary"
	case "github":
		coloDir = "vim-github-colorscheme"
	case "OceanicNext":
		coloDir = "oceanic-next"
	default:
		coloDir = colo
	}

	// Search colorscheme repo in dein.vim plugin directory
	//  and set the repository to runtimepath
	runtimeDir := ""
	for _, d := range lsDirs {
		dirname := d.Name()
		finfo, err := os.Stat(packpath + dirname)
		if err != nil {
			continue
		}
		if finfo.IsDir() {
			packDirs, _ := ioutil.ReadDir(packpath + dirname)
			for _, p := range packDirs {
				plugname := p.Name()
				if strings.Contains(plugname, coloDir) {
					runtimeDir = dirname + "/" + plugname
					break
				}
			}
			if runtimeDir != "" {
				break
			}
		}
	}
	m.nvim.Command("set runtimepath^=" + packpath + runtimeDir)
	m.nvim.Command("set runtimepath^=" + packpath + runtimeDir + "/colors")
	m.nvim.Command(":runtime! " + colo + ".vim")
	m.nvim.Command(":colorscheme " + colo)
	m.isSetColorscheme = true
}

func (m *MiniMap) mapScroll() {
	absScreenTop := m.ws.curLine - m.ws.screen.cursor[0]
	var absMapTop int
	m.nvim.Eval("line('w0')", &absMapTop)

	var regionHeight int
	var winpos [2]int
	for _, win := range m.ws.screen.curWins {
		if win.pos[0] <= m.ws.screen.cursor[0] && m.ws.screen.cursor[0] <= win.pos[0]+win.height {
			regionHeight = win.height
			winpos = win.pos
			break
		}
	}
	m.curRegion.SetFixedHeight(int(float64(regionHeight) * float64(m.font.lineHeight)))
	pos := int(float64(m.font.lineHeight) * float64(absScreenTop-absMapTop+winpos[0]))
	m.curRegion.Move2(0, pos)
}

func (m *MiniMap) handleRedraw(updates [][]interface{}) {
	for _, update := range updates {
		event := update[0].(string)
		args := update[1:]
		switch event {
		// case "update_fg":
		// case "update_bg":
		//	// go m.nvim.Command(`call rpcnotify(0, "Gui", "minimap_cursormoved",  getpos("."))`)
		// case "update_sp":
		case "cursor_goto":
			m.cursorGoto(args)
		case "put":
			m.put(args)
		case "eol_clear":
			m.eolClear(args)
		case "clear":
			m.clear(args)
		case "resize":
			m.resize(args)
		case "highlight_set":
			m.highlightSet(args)
		case "set_scroll_region":
			m.setScrollRegion(args)
		case "scroll":
			m.scroll(args)
			m.mapScroll()
		// case "mode_change":
		// case "popupmenu_show":
		// case "popupmenu_hide":
		// case "popupmenu_select":
		// case "tabline_update":
		// case "cmdline_show":
		// case "cmdline_pos":
		// case "cmdline_char":
		// case "cmdline_hide":
		// case "cmdline_function_show":
		// case "cmdline_function_hide":
		// case "wildmenu_show":
		// case "wildmenu_select":
		// case "wildmenu_hide":
		// case "msg_start_kind":
		// case "msg_chunk":
		// case "msg_end":
		// case "msg_showcmd":
		// case "messages":
		// case "busy_start":
		// case "busy_stop":
		default:
		}
	}
	m.update()
}

func (m *MiniMap) handleRPCGui(updates []interface{}) {
	event := updates[0].(string)
	switch event {
	// case "minimap_cursormoved":
	// 	pos := updates[1].([]interface{})
	// 	ln := util.ReflectToInt(pos[1])
	// 	m.curLine = ln
	default:
	}
}

func (m *MiniMap) put(args []interface{}) {
	numChars := 0
	x := m.cursor[1]
	y := m.cursor[0]
	row := m.cursor[0]
	col := m.cursor[1]
	if row >= m.rows {
		return
	}
	line := m.content[row]
	oldFirstNormal := true
	if x >= len(line) {
		x = len(line) - 1
	}
	cell := line[x] // sometimes crash at this line
	if cell != nil && !cell.normalWidth {
		oldFirstNormal = false
	}
	var lastCell *Cell
	oldNormalWidth := true
	for _, arg := range args {
		chars := arg.([]interface{})
		for _, c := range chars {
			if col >= len(line) {
				continue
			}
			cell := line[col]
			if cell != nil && !cell.normalWidth {
				oldNormalWidth = false
			} else {
				oldNormalWidth = true
			}
			if cell == nil {
				cell = &Cell{}
				line[col] = cell
			}
			cell.char = c.(string)
			cell.normalWidth = m.isNormalWidth(cell.char)
			lastCell = cell
			cell.highlight = m.highlight
			col++
			numChars++
		}
	}
	if lastCell != nil && !lastCell.normalWidth {
		numChars++
	}
	if !oldNormalWidth {
		numChars++
	}
	m.cursor[1] = col
	if x > 0 {
		cell := line[x-1]
		if cell != nil && cell.char != "" && !cell.normalWidth {
			x--
			numChars++
		} else {
			if !oldFirstNormal {
				x--
				numChars++
			}
		}
	}
	m.queueRedraw(x, y, numChars, 1)
}

func (m *MiniMap) highlightSet(args []interface{}) {
	for _, arg := range args {
		hl := arg.([]interface{})[0].(map[string]interface{})
		highlight := Highlight{}

		bold := hl["bold"]
		if bold != nil {
			highlight.bold = true
		} else {
			highlight.bold = false
		}

		italic := hl["italic"]
		if italic != nil {
			highlight.italic = true
		} else {
			highlight.italic = false
		}

		_, ok := hl["reverse"]
		if ok {
			highlight.foreground = m.highlight.background
			highlight.background = m.highlight.foreground
			m.highlight = highlight
			continue
		}

		fg, ok := hl["foreground"]
		if ok {
			rgba := calcColor(util.ReflectToInt(fg))
			highlight.foreground = rgba
		} else {
			highlight.foreground = m.foreground
		}

		bg, ok := hl["background"]
		if ok {
			rgba := calcColor(util.ReflectToInt(bg))
			highlight.background = rgba
		} else {
			highlight.background = m.background
		}
		m.highlight = highlight
	}
}

func (m *MiniMap) setScrollRegion(args []interface{}) {
	arg := args[0].([]interface{})
	top := util.ReflectToInt(arg[0])
	bot := util.ReflectToInt(arg[1])
	left := util.ReflectToInt(arg[2])
	right := util.ReflectToInt(arg[3])
	m.scrollRegion[0] = top
	m.scrollRegion[1] = bot
	m.scrollRegion[2] = left
	m.scrollRegion[3] = right
}

func (m *MiniMap) scroll(args []interface{}) {
	var count int
	var ucount uint
	switch args[0].([]interface{})[0].(type) {
	case int64:
		count = int(args[0].([]interface{})[0].(int64))
	case uint64:
		ucount = uint(args[0].([]interface{})[0].(uint64))
	}
	if ucount > 0 {
		count = int(ucount)
	}

	top := m.scrollRegion[0]
	bot := m.scrollRegion[1]
	left := m.scrollRegion[2]
	right := m.scrollRegion[3]

	if top == 0 && bot == 0 && left == 0 && right == 0 {
		top = 0
		bot = m.rows - 1
		left = 0
		right = m.cols - 1
	}

	m.queueRedraw(left, top, (right - left + 1), (bot - top + 1))

	if count > 0 {
		for row := top; row <= bot-count; row++ {
			for col := left; col <= right; col++ {
				if len(m.content) <= row+count {
					continue
				}
				for _, line := range m.content {
					if len(line) <= col {
						return
					}
				}
				m.content[row][col] = m.content[row+count][col]
			}
		}
		for row := bot - count + 1; row <= bot; row++ {
			for col := left; col <= right; col++ {
				m.content[row][col] = nil
			}
		}
		m.queueRedraw(left, (bot - count + 1), (right - left), count)
		if top > 0 {
			m.queueRedraw(left, (top - count), (right - left), count)
		}
	} else {
		for row := bot; row >= top-count; row-- {
			for col := left; col <= right; col++ {
				m.content[row][col] = m.content[row+count][col]
			}
		}
		for row := top; row < top-count; row++ {
			for col := left; col <= right; col++ {
				m.content[row][col] = nil
			}
		}
		m.queueRedraw(left, top, (right - left), -count)
		if bot < m.rows-1 {
			m.queueRedraw(left, bot+1, (right - left), -count)
		}
	}
}

func (m *MiniMap) update() {
	x := m.queueRedrawArea[0]
	y := m.queueRedrawArea[1]
	width := m.queueRedrawArea[2] - x
	height := m.queueRedrawArea[3] - y
	if width > 0 && height > 0 {
		// m.item.SetPixmap(s.pixmap)
		m.widget.Update2(
			int(float64(x)*m.font.truewidth),
			y*m.font.lineHeight,
			int(float64(width)*m.font.truewidth),
			height*m.font.lineHeight,
		)
	}
	m.queueRedrawArea[0] = m.cols
	m.queueRedrawArea[1] = m.rows
	m.queueRedrawArea[2] = 0
	m.queueRedrawArea[3] = 0
}

func (m *MiniMap) queueRedrawAll() {
	m.queueRedrawArea = [4]int{0, 0, m.cols, m.rows}
}

func (m *MiniMap) redraw() {
	m.queueRedrawArea = [4]int{m.cols, m.rows, 0, 0}
}

func (m *MiniMap) queueRedraw(x, y, width, height int) {
	if x < m.queueRedrawArea[0] {
		m.queueRedrawArea[0] = x
	}
	if y < m.queueRedrawArea[1] {
		m.queueRedrawArea[1] = y
	}
	if (x + width) > m.queueRedrawArea[2] {
		m.queueRedrawArea[2] = x + width
	}
	if (y + height) > m.queueRedrawArea[3] {
		m.queueRedrawArea[3] = y + height
	}
}

func (m *MiniMap) drawText(p *gui.QPainter, y int, col int, cols int, pos [2]int) {
	if y >= len(m.content) {
		return
	}
	font := p.Font()
	font.SetBold(false)
	font.SetItalic(false)
	pointF := core.NewQPointF()
	line := m.content[y]
	chars := map[Highlight][]int{}
	specialChars := []int{}
	if col > 0 {
		cell := line[col-1]
		if cell != nil && cell.char != "" {
			if !cell.normalWidth {
				col--
				cols++
			}
		}
	}
	if col+cols < m.cols {
	}
	for x := col; x < col+cols; x++ {
		if x >= len(line) {
			continue
		}
		cell := line[x]
		if cell == nil {
			continue
		}
		if cell.char == " " {
			continue
		}
		if cell.char == "" {
			continue
		}
		if !cell.normalWidth {
			specialChars = append(specialChars, x)
			continue
		}
		highlight := Highlight{}
		fg := cell.highlight.foreground
		if fg == nil {
			fg = m.foreground
		}
		highlight.foreground = fg
		highlight.italic = cell.highlight.italic
		highlight.bold = cell.highlight.bold

		colorSlice, ok := chars[highlight]
		if !ok {
			colorSlice = []int{}
		}
		colorSlice = append(colorSlice, x)
		chars[highlight] = colorSlice
	}
	for highlight, colorSlice := range chars {
		text := ""
		slice := colorSlice[:]
		for x := col; x < col+cols; x++ {
			if len(slice) == 0 {
				break
			}
			index := slice[0]
			if x < index {
				text += " "
				continue
			}
			if x == index {
				text += line[x].char
				slice = slice[1:]
			}
		}
		if text != "" {
			fg := highlight.foreground
			if fg != nil {
				p.SetPen2(gui.NewQColor3(fg.R, fg.G, fg.B, int(fg.A*255)))
			}
			pointF.SetX(float64(col-pos[1]) * m.font.truewidth)
			pointF.SetY(float64((y-pos[0])*m.font.lineHeight + m.font.shift))
			font.SetBold(highlight.bold)
			font.SetItalic(highlight.italic)
			p.DrawText(pointF, text)
		}
	}

	for _, x := range specialChars {
		cell := line[x]
		if cell == nil || cell.char == " " {
			continue
		}
		fg := cell.highlight.foreground
		if fg == nil {
			fg = m.foreground
		}
		p.SetPen2(gui.NewQColor3(fg.R, fg.G, fg.B, int(fg.A*255)))
		pointF.SetX(float64(x-pos[1]) * m.font.truewidth)
		pointF.SetY(float64((y-pos[0])*m.font.lineHeight + m.font.shift))
		font.SetBold(cell.highlight.bold)
		font.SetItalic(cell.highlight.italic)
		p.DrawText(pointF, cell.char)
	}
}

func (m *MiniMap) transparent(bg *RGBA) int {
	t := 255
	transparent := int(math.Trunc(editor.config.Editor.Transparent * float64(255)))

	if m.ws.background.equals(bg) {
		t = 0
	} else {
		t = transparent
	}
	return t
}

func (m *MiniMap) fillHightlight(p *gui.QPainter, y int, col int, cols int, pos [2]int) {
	rectF := core.NewQRectF()
	if y >= len(m.content) {
		return
	}
	line := m.content[y]
	start := -1
	end := -1
	var lastBg *RGBA
	var bg *RGBA
	var lastCell *Cell
	for x := col; x < col+cols; x++ {
		if x >= len(line) {
			continue
		}
		cell := line[x]
		if cell != nil {
			bg = cell.highlight.background
		} else {
			bg = nil
		}
		if lastCell != nil && !lastCell.normalWidth {
			bg = lastCell.highlight.background
		}
		if bg != nil {
			if lastBg == nil {
				start = x
				end = x
				lastBg = bg
			} else {
				if lastBg.equals(bg) {
					end = x
				} else {
					// last bg is different; draw the previous and start a new one
					rectF.SetRect(
						float64(start-pos[1])*m.font.truewidth,
						float64((y-pos[0])*m.font.lineHeight),
						float64(end-start+1)*m.font.truewidth,
						float64(m.font.lineHeight),
					)
					p.FillRect(
						rectF,
						gui.NewQBrush3(gui.NewQColor3(lastBg.R, lastBg.G, lastBg.B, m.transparent(lastBg)), core.Qt__SolidPattern),
					)

					// start a new one
					start = x
					end = x
					lastBg = bg
				}
			}
		} else {
			if lastBg != nil {
				rectF.SetRect(
					float64(start-pos[1])*m.font.truewidth,
					float64((y-pos[0])*m.font.lineHeight),
					float64(end-start+1)*m.font.truewidth,
					float64(m.font.lineHeight),
				)
				p.FillRect(
					rectF,
					gui.NewQBrush3(gui.NewQColor3(lastBg.R, lastBg.G, lastBg.B, m.transparent(lastBg)), core.Qt__SolidPattern),
				)

				// start a new one
				start = x
				end = x
				lastBg = nil
			}
		}
		lastCell = cell
	}
	if lastBg != nil {
		rectF.SetRect(
			float64(start-pos[1])*m.font.truewidth,
			float64((y-pos[0])*m.font.lineHeight),
			float64(end-start+1)*m.font.truewidth,
			float64(m.font.lineHeight),
		)
		p.FillRect(
			rectF,
			gui.NewQBrush3(gui.NewQColor3(lastBg.R, lastBg.G, lastBg.B, m.transparent(lastBg)), core.Qt__SolidPattern),
		)
	}
}

func (m *MiniMap) resize(args []interface{}) {
	m.cursor[0] = 0
	m.cursor[1] = 0
	m.content = make([][]*Cell, m.rows)
	for i := 0; i < m.rows; i++ {
		m.content[i] = make([]*Cell, m.cols)
	}
	m.queueRedrawAll()
}

func (m *MiniMap) clear(args []interface{}) {
	m.cursor[0] = 0
	m.cursor[1] = 0
	m.content = make([][]*Cell, m.rows)
	for i := 0; i < m.rows; i++ {
		m.content[i] = make([]*Cell, m.cols)
	}
	m.queueRedrawAll()
}

func (m *MiniMap) eolClear(args []interface{}) {
	row := m.cursor[0]
	col := m.cursor[1]
	if row >= m.rows {
		return
	}
	line := m.content[row]
	numChars := 0
	for x := col; x < len(line); x++ {
		line[x] = nil
		numChars++
	}
	m.queueRedraw(col, row, numChars+1, 1)
}

func (m *MiniMap) cursorGoto(args []interface{}) {
	pos, _ := args[0].([]interface{})
	m.cursor[0] = util.ReflectToInt(pos[0])
	m.cursor[1] = util.ReflectToInt(pos[1])
}

func (m *MiniMap) isNormalWidth(char string) bool {
	if len(char) == 0 {
		return true
	}
	if char[0] <= 127 {
		return true
	}
	//return s.ws.font.fontMetrics.Width(char) == s.ws.font.truewidth
	return m.font.fontMetrics.HorizontalAdvance(char, -1) == m.font.truewidth
}

func (m *MiniMap) wheelEvent(event *gui.QWheelEvent) {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()

	var v, h, vert, horiz int
	var accel int
	font := m.font

	switch runtime.GOOS {
	case "darwin":
		pixels := event.PixelDelta()
		if pixels != nil {
			v = pixels.Y()
			h = pixels.X()
		}
		if pixels.X() < 0 && m.scrollDust[0] > 0 {
			m.scrollDust[0] = 0
		}
		if pixels.Y() < 0 && m.scrollDust[1] > 0 {
			m.scrollDust[1] = 0
		}

		dx := math.Abs(float64(m.scrollDust[0]))
		dy := math.Abs(float64(m.scrollDust[1]))

		fontheight := float64(float64(font.lineHeight))
		fontwidth := float64(font.truewidth)

		m.scrollDust[0] += h
		m.scrollDust[1] += v

		if dx >= fontwidth {
			horiz = int(math.Trunc(float64(m.scrollDust[0]) / fontheight))
			m.scrollDust[0] = 0
		}
		if dy >= fontwidth {
			vert = int(math.Trunc(float64(m.scrollDust[1]) / fontwidth))
			m.scrollDust[1] = 0
		}

		m.scrollDustDeltaY = int(math.Abs(float64(vert)) - float64(m.scrollDustDeltaY))
		if m.scrollDustDeltaY < 1 {
			m.scrollDustDeltaY = 0
		}
		if m.scrollDustDeltaY <= 2 {
			accel = 1
		} else if m.scrollDustDeltaY > 2 {
			accel = int(float64(m.scrollDustDeltaY) / float64(4))
		}

	default:
		vert = event.AngleDelta().Y()
		accel = 16
	}

	if vert == 0 && horiz == 0 {
		return
	}

	if vert > 0 {
		m.nvim.Input(fmt.Sprintf("%v<C-y>", accel))
	} else if vert < 0 {
		m.nvim.Input(fmt.Sprintf("%v<C-e>", accel))
	}

	event.Accept()
}

func (m *MiniMap) mouseEvent(event *gui.QMouseEvent) {
	font := m.font
	y := int(float64(event.Y()) / float64(font.lineHeight))
	var absMapTop int
	m.nvim.Eval("line('w0')", &absMapTop)
	targetPos := absMapTop + y
	m.ws.nvim.Command(fmt.Sprintf("%d", targetPos))
}
