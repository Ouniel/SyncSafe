package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
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

type GitConfig struct {
	Platform    string // "gitee" 或 "github"
	RepoURL     string
	AccessToken string
	UserName    string
	UserEmail   string
	Enabled     bool
}

type BackupConfig struct {
	SourcePath      string
	DestinationPath string
	IsWatching      bool
	LastBackupTime  time.Time
	Git             GitConfig
}

type BackupApp struct {
	window       fyne.Window
	config       *BackupConfig
	statusBar    *widget.Label
	sourceLabel  *widget.Label
	destLabel    *widget.Label
	theme        *CustomTheme
	sourceFolder *widget.Label
	destFolder   *widget.Label
	watcher      *fsnotify.Watcher
	watchBtn     *widget.Button
	gitEnabled   *widget.Check
	backupMutex  sync.Mutex
	debounceTimer *time.Timer
	lastBackup    time.Time
}

// 自定义主题
type CustomTheme struct {
	fyne.Theme
}

func (t *CustomTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if name == theme.ColorNamePrimary {
		return color.NRGBA{R: 44, G: 193, B: 219, A: 255} // #2CC1DB
	}
	if name == theme.ColorNameHover {
		return color.NRGBA{R: 255, G: 107, B: 139, A: 255} // #FF6B8B
	}
	return t.Theme.Color(name, variant)
}

// 初始化 Git 仓库
func (b *BackupApp) initGitRepo() error {
	if b.config.Git.RepoURL == "" {
		return fmt.Errorf("Git 仓库地址不能为空")
	}

	if b.config.Git.UserName == "" || b.config.Git.UserEmail == "" {
		return fmt.Errorf("请先设置 Git 用户名和邮箱")
	}

	// 检查是否已经是 Git 仓库
	if _, err := os.Stat(filepath.Join(b.config.SourcePath, ".git")); err == nil {
		return nil // 已经是 Git 仓库
	}

	// 初始化 Git 仓库
	cmd := exec.Command("git", "init")
	cmd.Dir = b.config.SourcePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("初始化 Git 仓库失败: %v\n输出: %s", err, output)
	}

	// 配置 Git 用户信息
	cmds := []struct {
		name string
		args []string
	}{
		{"git", []string{"config", "--local", "user.name", b.config.Git.UserName}},
		{"git", []string{"config", "--local", "user.email", b.config.Git.UserEmail}},
		{"git", []string{"config", "--local", "init.defaultBranch", "master"}},
		{"git", []string{"remote", "add", "origin", b.config.Git.RepoURL}},
	}

	for _, c := range cmds {
		cmd := exec.Command(c.name, c.args...)
		cmd.Dir = b.config.SourcePath
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("Git 配置失败: %v\n命令: %s %v\n输出: %s", err, c.name, c.args, output)
		}
	}

	return nil
}

// 执行 Git 备份
func (b *BackupApp) gitBackup() error {
	if !b.config.Git.Enabled {
		return nil
	}

	// 清理可能存在的 Git 锁定文件
	gitDir := filepath.Join(b.config.SourcePath, ".git")
	lockFiles := []string{
		filepath.Join(gitDir, "index.lock"),
		filepath.Join(gitDir, "HEAD.lock"),
		filepath.Join(gitDir, "refs", "heads", "master.lock"),
	}
	for _, lockFile := range lockFiles {
		if _, err := os.Stat(lockFile); err == nil {
			if err := os.Remove(lockFile); err != nil {
				return fmt.Errorf("清理 Git 锁定文件失败: %v", err)
			}
		}
	}

	// 检查是否有变更
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = b.config.SourcePath
	output, err := statusCmd.Output()
	if err != nil {
		return fmt.Errorf("检查 Git 状态失败: %v", err)
	}

	// 如果没有变更，直接返回
	if len(output) == 0 {
		b.updateStatus("没有需要提交的更改")
		return nil
	}

	// Git 命令列表
	cmds := []struct {
		name string
		args []string
	}{
		{"git", []string{"add", "--all"}},
		{"git", []string{"commit", "-m", fmt.Sprintf("自动备份 - %s", time.Now().Format("2006-01-02 15:04:05"))}},
	}

	// 检查是否有远程仓库
	if output, err := exec.Command("git", "-C", b.config.SourcePath, "remote").Output(); err == nil && len(output) > 0 {
		// 添加 push 命令
		cmds = append(cmds, struct {
			name string
			args []string
		}{"git", []string{"push", "-u", "origin", "master"}})
	}

	// 设置环境变量
	env := os.Environ()
	if b.config.Git.AccessToken != "" {
		switch b.config.Git.Platform {
		case "GitHub":
			env = append(env, fmt.Sprintf("GIT_ASKPASS=echo %s", b.config.Git.AccessToken))
		case "Gitee":
			env = append(env, fmt.Sprintf("GITEE_TOKEN=%s", b.config.Git.AccessToken))
		}
	}

	// 执行 Git 命令
	for _, c := range cmds {
		cmd := exec.Command(c.name, c.args...)
		cmd.Dir = b.config.SourcePath
		cmd.Env = env

		// 执行命令并捕获输出
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s 失败: %v\n输出: %s", c.args[0], err, string(output))
		}

		// 更新状态
		b.updateStatus(fmt.Sprintf("Git %s 成功", c.args[0]))
	}

	return nil
}

// 显示 Git 配置对话框
func (b *BackupApp) showGitConfigDialog() {
	// 创建平台选择下拉框
	platformSelect := widget.NewSelect([]string{"Gitee", "GitHub"}, func(platform string) {
		b.config.Git.Platform = platform
	})
	platformSelect.SetSelected(b.config.Git.Platform)

	// 创建用户名输入框
	userNameEntry := widget.NewEntry()
	userNameEntry.SetPlaceHolder("输入 Git 用户名")
	userNameEntry.SetText(b.config.Git.UserName)
	userNameEntry.OnChanged = func(name string) {
		b.config.Git.UserName = name
	}

	// 创建邮箱输入框
	userEmailEntry := widget.NewEntry()
	userEmailEntry.SetPlaceHolder("输入 Git 邮箱")
	userEmailEntry.SetText(b.config.Git.UserEmail)
	userEmailEntry.OnChanged = func(email string) {
		b.config.Git.UserEmail = email
	}

	// 创建仓库地址输入框
	repoEntry := widget.NewEntry()
	repoEntry.SetPlaceHolder("输入仓库 HTTPS 地址")
	repoEntry.SetText(b.config.Git.RepoURL)
	repoEntry.OnChanged = func(url string) {
		b.config.Git.RepoURL = url
	}

	// 创建访问令牌输入框
	tokenEntry := widget.NewPasswordEntry()
	tokenEntry.SetPlaceHolder("输入访问令牌 (Access Token)")
	tokenEntry.SetText(b.config.Git.AccessToken)
	tokenEntry.OnChanged = func(token string) {
		b.config.Git.AccessToken = token
	}

	// 创建启用 Git 备份复选框
	gitEnabled := widget.NewCheck("启用 Git 备份", func(enabled bool) {
		b.config.Git.Enabled = enabled
	})
	gitEnabled.Checked = b.config.Git.Enabled

	// 创建表单布局
	form := &widget.Form{
		Items: []*widget.FormItem{
			{
				Text:     "Git 平台",
				Widget:   platformSelect,
				HintText: "选择 Git 托管平台",
			},
			{
				Text:     "用户名",
				Widget:   userNameEntry,
				HintText: "您的 Git 用户名",
			},
			{
				Text:     "邮箱",
				Widget:   userEmailEntry,
				HintText: "您的 Git 邮箱地址",
			},
			{
				Text:     "仓库地址",
				Widget:   repoEntry,
				HintText: "仓库的 HTTPS 克隆地址",
			},
			{
				Text:     "访问令牌",
				Widget:   tokenEntry,
				HintText: "用于身份验证的访问令牌",
			},
		},
	}

	// 创建帮助信息
	helpText := widget.NewRichTextFromMarkdown(`
### Git 配置说明

#### 1. 平台选择
- 支持 Gitee 和 GitHub
- 请选择您已注册的平台

#### 2. 基本信息
- **用户名**: Git 提交时显示的作者名
- **邮箱**: Git 提交关联的邮箱地址

#### 3. 仓库配置
- **仓库地址**: 使用 HTTPS 格式
  - Gitee 格式: https://gitee.com/用户名/仓库名.git
  - GitHub 格式: https://github.com/用户名/仓库名.git

#### 4. 访问令牌
- **Gitee**: 在 设置 -> 私人令牌 中生成
- **GitHub**: 在 Settings -> Developer settings -> Personal access tokens 中生成
- 确保令牌具有仓库的读写权限
`)

	// 创建标题
	title := container.NewHBox(
		widget.NewIcon(theme.SettingsIcon()),
		widget.NewLabelWithStyle("Git 备份配置", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
	)

	// 创建主内容
	content := container.NewVBox(
		title,
		widget.NewSeparator(),
		container.NewPadded(form),
		container.NewPadded(gitEnabled),
		widget.NewSeparator(),
		container.NewPadded(helpText),
	)

	// 包装在滚动容器中
	scrollContent := container.NewVScroll(content)
	scrollContent.SetMinSize(fyne.NewSize(500, 400))

	// 创建自定义对话框
	dialog.ShowCustomConfirm("Git 配置", "确定", "取消", scrollContent,
		func(submit bool) {
			if !submit {
				return
			}

			// 验证必填字段
			if b.config.Git.Enabled {
				if b.config.Git.Platform == "" {
					dialog.ShowError(fmt.Errorf("请选择 Git 平台"), b.window)
					return
				}
				if b.config.Git.UserName == "" {
					dialog.ShowError(fmt.Errorf("请输入 Git 用户名"), b.window)
					return
				}
				if b.config.Git.UserEmail == "" {
					dialog.ShowError(fmt.Errorf("请输入 Git 邮箱"), b.window)
					return
				}
				if b.config.Git.RepoURL == "" {
					dialog.ShowError(fmt.Errorf("请输入仓库地址"), b.window)
					return
				}
				if b.config.Git.AccessToken == "" {
					dialog.ShowError(fmt.Errorf("请输入访问令牌"), b.window)
					return
				}

				// 保存配置
				if err := b.saveConfig(); err != nil {
					dialog.ShowError(fmt.Errorf("保存配置失败: %v", err), b.window)
					return
				}

				// 初始化 Git 仓库
				if err := b.initGitRepo(); err != nil {
					dialog.ShowError(fmt.Errorf("Git 仓库初始化失败: %v", err), b.window)
					return
				}

				b.updateStatus("Git 配置已更新")
			}
		}, b.window)
}

// 保存配置到文件
func (b *BackupApp) saveConfig() error {
	configDir := filepath.Join(".", "syncsafe")
	configPath := filepath.Join(configDir, "config.json")

	// 创建配置目录
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %v", err)
	}

	// 序列化配置
	data, err := json.MarshalIndent(b.config, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %v", err)
	}

	// 写入文件
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}

	return nil
}

func newBackupApp() *BackupApp {
	app := &BackupApp{
		config: &BackupConfig{
			IsWatching: false,
			Git: GitConfig{
				Enabled: false,
			},
		},
		statusBar:   widget.NewLabelWithStyle("准备就绪", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		sourceLabel: widget.NewLabel("未选择源文件夹"),
		destLabel:   widget.NewLabel("未选择目标文件夹"),
		theme:       &CustomTheme{Theme: theme.DefaultTheme()},
	}
	return app
}

func (b *BackupApp) createUI() {
	// 设置窗口标题和图标
	b.window.SetTitle("SyncSafe 文件备份工具")
	b.window.Resize(fyne.NewSize(500, 400))

	// 创建标题容器
	titleContainer := container.NewVBox(
		container.NewHBox(
			layout.NewSpacer(),
			widget.NewIcon(theme.StorageIcon()),
			widget.NewLabelWithStyle("SyncSafe", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
			layout.NewSpacer(),
		),
		container.NewHBox(
			layout.NewSpacer(),
			widget.NewLabelWithStyle("文件备份工具", fyne.TextAlignCenter, fyne.TextStyle{}),
			layout.NewSpacer(),
		),
	)

	// 初始化标签
	b.sourceFolder = widget.NewLabel("未选择源文件夹")
	b.destFolder = widget.NewLabel("未选择目标文件夹")

	// 创建源文件夹选择按钮和显示
	sourceBtn := widget.NewButtonWithIcon("选择源文件夹", customFolderIcon, func() {
		b.showFolderDialog("选择源文件夹", func(path string) {
			if path == "" {
				return
			}
			b.config.SourcePath = path
			b.sourceLabel.SetText(path)
			b.updateStatus("已选择源文件夹: " + path)
			b.sourceFolder.SetText(path)
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
			b.destFolder.SetText(path)
		})
	})
	destBtn.Importance = widget.HighImportance

	// 创建监控按钮
	b.watchBtn = widget.NewButton("开始监控", func() {
		if !b.config.IsWatching {
			if err := b.startWatching(); err != nil {
				dialog.ShowError(err, b.window)
				return
			}
			b.watchBtn.SetText("停止监控")
			b.watchBtn.Icon = theme.MediaStopIcon()
		} else {
			b.stopWatching()
			b.watchBtn.SetText("开始监控")
			b.watchBtn.Icon = theme.MediaPlayIcon()
		}
	})
	b.watchBtn.Icon = theme.MediaPlayIcon()

	// 创建备份按钮
	backupBtn := widget.NewButtonWithIcon("立即备份", theme.MailSendIcon(), func() {
		go b.performBackup()
	})
	backupBtn.Importance = widget.HighImportance

	// 添加 Git 备份选项
	b.gitEnabled = widget.NewCheck("启用 Git 备份", func(value bool) {
		b.config.Git.Enabled = value
	})
	b.gitEnabled.Checked = b.config.Git.Enabled

	// 创建 Git 配置按钮
	gitConfigBtn := widget.NewButton("Git 配置", func() {
		b.showGitConfigDialog()
	})
	gitConfigBtn.Icon = theme.SettingsIcon()

	// 创建文件夹信息区域
	folderInfo := container.NewVBox(
		container.NewHBox(
			widget.NewIcon(customFolderIcon),
			widget.NewLabel("源文件夹:"),
		),
		container.NewPadded(
			b.sourceFolder,
		),
		layout.NewSpacer(),
		container.NewHBox(
			widget.NewIcon(customFolderIcon),
			widget.NewLabel("目标文件夹:"),
		),
		container.NewPadded(
			b.destFolder,
		),
	)

	// 创建按钮组
	buttonGroup := container.NewVBox(
		container.NewGridWithColumns(2,
			container.NewPadded(sourceBtn),
			container.NewPadded(destBtn),
		),
		container.NewHBox(
			container.NewHBox(b.gitEnabled, gitConfigBtn),
			layout.NewSpacer(),
			b.watchBtn,
			backupBtn,
		),
	)

	// 创建状态栏
	statusBar := container.NewHBox(
		widget.NewIcon(theme.InfoIcon()),
		b.statusBar,
	)

	// 创建主布局
	content := container.NewVBox(
		container.NewPadded(titleContainer),
		widget.NewSeparator(),
		buttonGroup,
		widget.NewSeparator(),
		container.NewPadded(
			container.NewVBox(
				container.NewHBox(
					widget.NewIcon(theme.FolderIcon()),
					widget.NewLabelWithStyle("文件夹信息", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				),
				folderInfo,
			),
		),
		widget.NewSeparator(),
		container.NewPadded(
			container.NewVBox(
				container.NewHBox(
					widget.NewIcon(theme.InfoIcon()),
					widget.NewLabelWithStyle("状态信息", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				),
				statusBar,
			),
		),
	)

	// 设置主窗口内容
	b.window.SetContent(content)
}

func (b *BackupApp) updateStatus(message string) {
	b.statusBar.SetText(message)
}

func (b *BackupApp) startWatching() error {
	if b.config.SourcePath == "" {
		return fmt.Errorf("请先选择源文件夹")
	}

	if b.config.DestinationPath == "" {
		return fmt.Errorf("请先选择目标文件夹")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建监控失败: %v", err)
	}

	// 递归添加所有子目录
	err = filepath.Walk(b.config.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// 跳过.git目录
			if filepath.Base(path) == ".git" {
				return filepath.SkipDir
			}
			err = watcher.Add(path)
			if err != nil {
				return fmt.Errorf("添加监控目录失败 %s: %v", path, err)
			}
		}
		return nil
	})

	if err != nil {
		watcher.Close()
		return fmt.Errorf("设置监控失败: %v", err)
	}

	b.watcher = watcher
	b.config.IsWatching = true

	// 启动监控协程
	go func() {
		const debounceDelay = 5 * time.Second // 防抖动延迟时间
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write ||
					event.Op&fsnotify.Create == fsnotify.Create ||
					event.Op&fsnotify.Remove == fsnotify.Remove ||
					event.Op&fsnotify.Rename == fsnotify.Rename {
					// 实现防抖动：取消之前的定时器（如果存在）
					if b.debounceTimer != nil {
						b.debounceTimer.Stop()
					}

					// 创建新的定时器
					b.debounceTimer = time.AfterFunc(debounceDelay, func() {
						// 检查距离上次备份的时间间隔
						if time.Since(b.lastBackup) < debounceDelay {
							return
						}
						// 尝试获取互斥锁
						if !b.backupMutex.TryLock() {
							b.updateStatus("已有备份正在进行中...")
							return
						}
						defer b.backupMutex.Unlock()
						b.performBackup()
						b.lastBackup = time.Now()
					})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("监控错误: %v", err)
			}
		}
	}()

	b.updateStatus("开始监控文件变化")
	return nil
}

func (b *BackupApp) stopWatching() {
	if b.watcher != nil {
		b.watcher.Close()
		b.watcher = nil
	}
	b.config.IsWatching = false
	b.updateStatus("停止监控")
}

func (b *BackupApp) copyFile(src, dst string) error {
	// 获取源文件信息
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("获取源文件信息失败: %v", err)
	}

	// 如果目标文件已存在，检查是否需要更新
	if dstInfo, err := os.Stat(dst); err == nil {
		if dstInfo.ModTime().Equal(srcInfo.ModTime()) {
			return nil // 文件未修改，无需复制
		}
	}

	// 确保目标目录存在
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("创建目标目录失败: %v", err)
	}

	// 尝试打开源文件
	var source *os.File
	for retries := 0; retries < 3; retries++ {
		source, err = os.Open(src)
		if err == nil {
			break
		}
		time.Sleep(time.Second) // 等待一秒后重试
	}
	if err != nil {
		return fmt.Errorf("打开源文件失败: %v", err)
	}
	defer source.Close()

	// 生成临时文件名（不包含空格）
	tmpFile := filepath.Join(
		filepath.Dir(dst),
		fmt.Sprintf("%s.tmp_%d",
			strings.ReplaceAll(filepath.Base(dst), " ", "_"),
			time.Now().UnixNano(),
		),
	)

	// 创建临时文件
	var destination *os.File
	for retries := 0; retries < 3; retries++ {
		destination, err = os.Create(tmpFile)
		if err == nil {
			break
		}
		time.Sleep(time.Second) // 等待一秒后重试
	}
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %v", err)
	}

	// 使用defer和匿名函数来确保在出错时删除临时文件
	defer func() {
		destination.Close()
		if err != nil {
			os.Remove(tmpFile)
		}
	}()

	// 复制文件内容
	if _, err = io.Copy(destination, source); err != nil {
		return fmt.Errorf("复制文件内容失败: %v", err)
	}

	// 确保文件内容已写入磁盘
	if err = destination.Sync(); err != nil {
		return fmt.Errorf("同步文件内容失败: %v", err)
	}

	// 关闭目标文件
	if err = destination.Close(); err != nil {
		return fmt.Errorf("关闭目标文件失败: %v", err)
	}

	// 设置文件权限和时间戳
	if err = os.Chmod(tmpFile, srcInfo.Mode()); err != nil {
		return fmt.Errorf("设置文件权限失败: %v", err)
	}

	// 设置修改时间
	if err = os.Chtimes(tmpFile, time.Now(), srcInfo.ModTime()); err != nil {
		return fmt.Errorf("设置文件时间失败: %v", err)
	}

	// 如果目标文件存在，先尝试删除
	if _, err := os.Stat(dst); err == nil {
		for retries := 0; retries < 3; retries++ {
			err = os.Remove(dst)
			if err == nil {
				break
			}
			time.Sleep(time.Second) // 等待一秒后重试
		}
		if err != nil {
			os.Remove(tmpFile) // 清理临时文件
			return fmt.Errorf("删除已存在的目标文件失败: %v", err)
		}
	}

	// 确保目标目录存在
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		os.Remove(tmpFile) // 清理临时文件
		return fmt.Errorf("创建目标目录失败: %v", err)
	}

	// 重命名临时文件为最终文件
	for retries := 0; retries < 3; retries++ {
		err = os.Rename(tmpFile, dst)
		if err == nil {
			break
		}
		time.Sleep(time.Second) // 等待一秒后重试
	}
	if err != nil {
		os.Remove(tmpFile) // 清理临时文件
		return fmt.Errorf("重命名文件失败: %v\n源文件: %s\n目标文件: %s", err, tmpFile, dst)
	}

	return nil
}

func (b *BackupApp) performBackup() {
	if b.config.SourcePath == "" || b.config.DestinationPath == "" {
		dialog.ShowError(fmt.Errorf("请先选择源文件夹和备份文件夹"), b.window)
		return
	}

	// 验证源文件夹是否存在
	if _, err := os.Stat(b.config.SourcePath); err != nil {
		dialog.ShowError(fmt.Errorf("源文件夹不存在或无法访问: %v", err), b.window)
		return
	}

	b.updateStatus("开始备份...")

	// 如果启用了 Git 备份，先执行 Git 操作
	if b.config.Git.Enabled {
		if err := b.gitBackup(); err != nil {
			dialog.ShowError(fmt.Errorf("Git 备份失败: %v", err), b.window)
			return
		}
		b.updateStatus("Git 备份完成")
	}

	// 创建本地备份文件夹（替换空格为下划线）
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	folderName := strings.ReplaceAll(filepath.Base(b.config.SourcePath), " ", "_") + "-" + timestamp
	backupDir := filepath.Join(filepath.Clean(b.config.DestinationPath), folderName)

	// 确保父目录存在
	parentDir := filepath.Dir(backupDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		dialog.ShowError(fmt.Errorf("创建父目录失败: %v\n目录: %s", err, parentDir), b.window)
		return
	}

	// 创建备份目录
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		dialog.ShowError(fmt.Errorf("创建备份目录失败: %v\n目录: %s", err, backupDir), b.window)
		return
	}

	// 遍历源文件夹
	err := filepath.Walk(b.config.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("访问文件失败: %v\n文件: %s", err, path)
		}

		// 跳过 .git 目录
		if info.IsDir() && filepath.Base(path) == ".git" {
			return filepath.SkipDir
		}

		// 获取相对路径
		relPath, err := filepath.Rel(b.config.SourcePath, path)
		if err != nil {
			return fmt.Errorf("计算相对路径失败: %v\n文件: %s", err, path)
		}

		// 构建目标路径（替换空格为下划线）
		destPath := filepath.Join(backupDir, strings.ReplaceAll(relPath, " ", "_"))

		// 如果是目录，创建对应的目录
		if info.IsDir() {
			if err := os.MkdirAll(destPath, info.Mode()); err != nil {
				return fmt.Errorf("创建目录失败: %v\n目录: %s", err, destPath)
			}
			return nil
		}

		// 复制文件
		if err := b.copyFile(path, destPath); err != nil {
			return fmt.Errorf("复制文件失败: %v\n源文件: %s\n目标文件: %s", err, path, destPath)
		}

		return nil
	})

	if err != nil {
		dialog.ShowError(fmt.Errorf("备份失败: %v", err), b.window)
		return
	}

	b.updateStatus("备份完成")
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
	myApp.Settings().SetTheme(&CustomTheme{Theme: theme.DefaultTheme()})
	myApp.SetIcon(theme.StorageIcon())

	window := myApp.NewWindow("SyncSafe 文件备份工具")
	window.Resize(fyne.NewSize(500, 400))
	window.SetIcon(theme.StorageIcon())

	backupApp := newBackupApp()
	backupApp.window = window
	backupApp.createUI()

	window.ShowAndRun()
}
