package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/fsnotify/fsnotify"
)

var customFolderIcon fyne.Resource

func init() {
	iconBytes, err := os.ReadFile("assets/folder.svg")
	if err != nil {
		log.Printf("Warning: Could not load custom folder icon: %v", err)
		customFolderIcon = theme.FolderIcon()
		return
	}
	customFolderIcon = &fyne.StaticResource{
		StaticName:    "folder",
		StaticContent: iconBytes,
	}
}

type BackupConfig struct {
	SourcePath      string
	DestinationPath string
	IsWatching      bool
	LastBackupTime  time.Time
}

type BackupApp struct {
	config      BackupConfig
	window      fyne.Window
	watcher     *fsnotify.Watcher
	statusBar   *widget.Label
	watchBtn    *widget.Button
	sourceLabel *widget.Label
	destLabel   *widget.Label
}

func newBackupApp() *BackupApp {
	return &BackupApp{
		config: BackupConfig{
			IsWatching: false,
		},
		statusBar:   widget.NewLabelWithStyle("准备就绪", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		sourceLabel: widget.NewLabel("未选择源文件夹"),
		destLabel:   widget.NewLabel("未选择目标文件夹"),
	}
}

func (b *BackupApp) createUI() {
	// 创建标题
	titleStyle := fyne.TextStyle{Bold: true}
	title := widget.NewLabelWithStyle("SyncSafe 文件备份工具", fyne.TextAlignCenter, titleStyle)

	// 创建源文件夹选择按钮和显示
	sourceBtn := widget.NewButtonWithIcon("选择源文件夹", customFolderIcon, func() {
		b.showFolderDialog("选择源文件夹", func(path string) {
			if path == "" {
				return
			}
			b.config.SourcePath = path
			b.sourceLabel.SetText(path)
			b.updateStatus("已选择源文件夹: " + path)
		})
	})
	sourceBtn.Importance = widget.HighImportance

	// 创建目标文件夹选择按钮和显示
	destBtn := widget.NewButtonWithIcon("选择备份文件夹", customFolderIcon, func() {
		b.showFolderDialog("选择备份文件夹", func(path string) {
			if path == "" {
				return
			}
			b.config.DestinationPath = path
			b.destLabel.SetText(path)
			b.updateStatus("已选择备份文件夹: " + path)
		})
	})
	destBtn.Importance = widget.HighImportance

	// 创建开始/停止监控按钮
	watchIcon := theme.MediaPlayIcon()
	b.watchBtn = widget.NewButtonWithIcon("开始监控", watchIcon, func() {
		if b.config.IsWatching {
			b.stopWatching()
			b.watchBtn.SetIcon(theme.MediaPlayIcon())
			b.watchBtn.SetText("开始监控")
		} else {
			b.startWatching()
			b.watchBtn.SetIcon(theme.MediaPauseIcon())
			b.watchBtn.SetText("停止监控")
		}
	})

	// 创建立即备份按钮
	backupIcon := theme.DownloadIcon()
	backupBtn := widget.NewButtonWithIcon("立即备份", backupIcon, func() {
		b.performBackup()
	})
	backupBtn.Importance = widget.HighImportance

	// 创建状态面板
	statusIcon := theme.InfoIcon()
	statusTitle := container.NewHBox(
		widget.NewIcon(statusIcon),
		widget.NewLabelWithStyle("状态信息", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
	)
	statusPanel := container.NewVBox(
		statusTitle,
		b.statusBar,
	)

	// 创建文件夹信息面板
	infoIcon := theme.FolderIcon()
	folderTitle := container.NewHBox(
		widget.NewIcon(infoIcon),
		widget.NewLabelWithStyle("文件夹信息", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
	)
	folderInfo := container.NewVBox(
		folderTitle,
		b.sourceLabel,
		b.destLabel,
	)

	// 创建按钮面板
	buttonPanel := container.NewGridWithColumns(2,
		container.NewPadded(sourceBtn),
		container.NewPadded(destBtn),
		container.NewPadded(b.watchBtn),
		container.NewPadded(backupBtn),
	)

	// 创建主布局
	content := container.NewVBox(
		container.NewPadded(title),
		widget.NewSeparator(),
		container.NewPadded(buttonPanel),
		widget.NewSeparator(),
		container.NewPadded(folderInfo),
		widget.NewSeparator(),
		container.NewPadded(statusPanel),
	)

	// 设置主窗口内容
	b.window.SetContent(container.NewPadded(content))
}

func (b *BackupApp) updateStatus(message string) {
	b.statusBar.SetText(message)
}

func (b *BackupApp) startWatching() {
	if b.config.SourcePath == "" || b.config.DestinationPath == "" {
		dialog.ShowError(fmt.Errorf("请先选择源文件夹和备份文件夹"), b.window)
		return
	}

	var err error
	b.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		dialog.ShowError(err, b.window)
		return
	}

	// 递归添加所有子目录到监控
	err = filepath.Walk(b.config.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return b.watcher.Add(path)
		}
		return nil
	})

	if err != nil {
		dialog.ShowError(err, b.window)
		return
	}

	b.config.IsWatching = true
	b.updateStatus("正在监控文件变化...")

	// 启动监控协程
	go func() {
		for {
			select {
			case event, ok := <-b.watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					b.performBackup()
				}
			case err, ok := <-b.watcher.Errors:
				if !ok {
					return
				}
				log.Println("监控错误:", err)
			}
		}
	}()
}

func (b *BackupApp) stopWatching() {
	if b.watcher != nil {
		b.watcher.Close()
		b.config.IsWatching = false
		b.updateStatus("监控已停止")
	}
}

func (b *BackupApp) performBackup() {
	if b.config.SourcePath == "" || b.config.DestinationPath == "" {
		dialog.ShowError(fmt.Errorf("请先选择源文件夹和备份文件夹"), b.window)
		return
	}

	b.updateStatus("正在备份...")

	// 获取源文件夹名称
	sourceFolderName := filepath.Base(b.config.SourcePath)
	// 创建备份时间戳文件夹
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	backupDir := filepath.Join(b.config.DestinationPath, fmt.Sprintf("%s-%s", sourceFolderName, timestamp))

	err := filepath.Walk(b.config.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// 计算相对路径
		relPath, err := filepath.Rel(b.config.SourcePath, path)
		if err != nil {
			return err
		}

		destPath := filepath.Join(backupDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(destPath, os.ModePerm)
		}

		// 复制文件
		input, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		err = os.MkdirAll(filepath.Dir(destPath), os.ModePerm)
		if err != nil {
			return err
		}

		return os.WriteFile(destPath, input, os.ModePerm)
	})

	if err != nil {
		dialog.ShowError(err, b.window)
		b.updateStatus("备份失败")
		return
	}

	b.config.LastBackupTime = time.Now()
	b.updateStatus(fmt.Sprintf("备份完成: %s", timestamp))
}

func (b *BackupApp) showFolderDialog(title string, callback func(string)) {
	// 创建一个新窗口作为对话框
	customDialog := dialog.NewCustom(title, "取消",
		container.NewVBox(
			widget.NewLabel("请选择文件夹:"),
			container.NewHBox(
				widget.NewIcon(customFolderIcon),
				widget.NewLabel("点击\"选择\"按钮浏览文件夹"),
			),
		),
		b.window,
	)

	// 添加确认按钮
	confirmBtn := widget.NewButton("选择", nil)
	customDialog.SetButtons([]fyne.CanvasObject{confirmBtn})

	// 设置确认按钮动作
	confirmBtn.OnTapped = func() {
		// 使用标准的文件夹选择对话框
		dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
			if err != nil {
				dialog.ShowError(err, b.window)
				return
			}
			if lu == nil {
				return
			}
			callback(lu.Path())
			customDialog.Hide()
		}, b.window)
	}

	// 显示对话框
	customDialog.Show()
}

func main() {
	myApp := app.New()
	myApp.SetIcon(theme.StorageIcon())

	window := myApp.NewWindow("SyncSafe 文件备份工具")
	window.Resize(fyne.NewSize(500, 400))
	window.SetIcon(theme.StorageIcon())

	backupApp := newBackupApp()
	backupApp.window = window
	backupApp.createUI()

	window.ShowAndRun()
}
