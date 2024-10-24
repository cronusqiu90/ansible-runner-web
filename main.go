package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/apenella/go-ansible/v2/pkg/execute"
	results "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"github.com/apenella/go-ansible/v2/pkg/execute/stdoutcallback"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
)

type User struct {
	ID       uint   `json:"id" gorm:"primarykey"`
	Name     string `json:"name" gorm:"column:name"`
	Password string `json:"password" gorm:"column:password"`
}

type Inventory struct {
	ID      uint   `json:"id" gorm:"primarykey"`
	Name    string `json:"name" gorm:"column:name"`
	Path    string `json:"path" gorm:"column:path"`
	Creator string `json:"creator" gorm:"column:creator"`
}

type Playbook struct {
	ID      uint   `json:"id" gorm:"primarykey"`
	Name    string `json:"name" gorm:"column:name"`
	Path    string `json:"path" gorm:"column:path"`
	Creator string `json:"creator" gorm:"column:creator"`
}

type Task struct {
	ID          uint      `json:"id" gorm:"primarykey"`
	TaskID      string    `json:"task_id" gorm:"column:task_id"`
	Name        string    `json:"name" gorm:"column:name"`
	Status      uint      `json:"status" gorm:"column:status"`
	UpdatedAt   time.Time `json:"updated_at" gorm:"column:updated_at"`
	PlaybookID  uint      `gorm:"column:playbook_id"`
	Playbook    Playbook  `gorm:"foreignKey:PlaybookID;references:ID"`
	InventoryID uint      `gorm:"column:inventory_id"`
	Inventory   Inventory `gorm:"foreignKey:InventoryID;references:ID"`
	UserID      uint      `gorm:"column:user_id"`
	User        User      `gorm:"foreignKey:UserID;references:ID"`
	Error       string    `json:"error" gorm:"column:error"`
}

const (
	SSH_USER_PRI_KEY_FILE = "/home/user/.ssh/id_rsa"
	SSH_USER              = "user"
	SSH_PORT              = 22
)

var (
	//go:embed templates/*.html
	fs embed.FS

	address  string
	db       *gorm.DB
	rootDir  string
	taskChan = make(chan string)
	stopChan = make(chan struct{})
)

func init() {
	cwd, _ := os.Getwd()
	rootDir = filepath.Join(cwd, "data")
	if _, err := os.Stat(rootDir); os.IsNotExist(err) {
		os.Mkdir(rootDir, 0755)
	}
	flag.StringVar(&address, "s", "0.0.0.0:17000", "address to listen on")
}

func main() {
	flag.Parse()

	setupDB()

	r := gin.Default()
	templ := template.Must(template.New("").ParseFS(fs, "templates/*.html"))
	r.SetHTMLTemplate(templ)
	r.GET("/", showIndex)
	r.GET("/task", func(c *gin.Context) {
		c.HTML(http.StatusOK, "createTask.html", gin.H{})
	})
	r.GET("/task/:id", showTask)
	r.POST("/task", createTask)
	r.GET("/result/:id", showResult)
	r.GET("/runTask/:id", func(c *gin.Context) {
		taskId := c.Param("id")
		taskChan <- taskId
		c.Redirect(302, "/")
	})

	srv := &http.Server{Addr: address, Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	wait := sync.WaitGroup{}
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go startRunAnsiblePlaybookService(i, &wait)
	}

	//
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	go func() {
		name := <-quit
		log.Printf("Warn: received signal: %v\n", name)
		close(taskChan)
	}()

	wait.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}
	log.Println("Server exiting")
}

func setupDB() {
	var err error
	db, err = gorm.Open(sqlite.Open("data.db"), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}

	if err := db.AutoMigrate(
		&User{}, &Inventory{}, &Playbook{}, &Task{},
	); err != nil {
		log.Fatalf("failed to migrate: %v", err)
	}
}

func showIndex(c *gin.Context) {
	var tasks []Task
	tx := db.Preload("Playbook").Preload("Inventory").Preload("User").Order("id desc").Limit(10).Find(&tasks)

	if tx.Error != nil {
		c.JSON(400, gin.H{"error": tx.Error.Error()})
	}
	c.HTML(http.StatusOK, "index.html", gin.H{
		"tasks": tasks,
	})
}

func showTask(c *gin.Context) {
	taskId := c.Param("id")
	var task Task
	db.Preload("Playbook").Preload("Inventory").Preload("User").First(&task, "task_id = ?", taskId)

	var playbookContent, inventoryContent string
	var err error
	playbookContent, err = readFile(task.Playbook.Path)
	if err != nil {
		playbookContent = err.Error()
	}
	inventoryContent, err = readFile(task.Inventory.Path)
	if err != nil {
		inventoryContent = err.Error()
	}

	c.IndentedJSON(http.StatusOK, gin.H{
		"task":      task,
		"playbook":  playbookContent,
		"inventory": inventoryContent,
	})
}

func createTask(c *gin.Context) {
	taskName := c.PostForm("name")
	playbookContent := c.PostForm("playbook")
	inventoryContent := c.PostForm("inventory")
	taskID := uuid.New().String()

	var w bytes.Buffer
	w.WriteString("- hosts: servers\n")
	w.WriteString("  gather_facts: false\n")
	w.WriteString("  tasks:\n")
	playbookContent = strings.ReplaceAll(playbookContent, "\r", "")
	for _, v := range strings.Split(playbookContent, "\n") {
		w.WriteString("  " + v + "\n")
	}

	playbookPath := filepath.Join(rootDir, taskID, "site.yaml")
	if err := writeFile(playbookPath, w.String()); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	playbook := Playbook{
		Name:    taskName,
		Path:    playbookPath,
		Creator: "admin",
	}
	if err := db.Create(&playbook).Error; err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	w.Reset()
	w.WriteString("[servers]\n")
	w.WriteString(inventoryContent)

	inventoryPath := filepath.Join(rootDir, taskID, "inventory.ini")
	if err := writeFile(inventoryPath, w.String()); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
	}
	inventory := Inventory{
		Name:    taskName,
		Path:    inventoryPath,
		Creator: "admin",
	}
	if err := db.Create(&inventory).Error; err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	task := Task{
		TaskID:      taskID,
		Name:        taskName,
		Status:      0,
		PlaybookID:  playbook.ID,
		InventoryID: inventory.ID,
		UserID:      1,
	}
	if err := db.Create(&task).Error; err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	c.IndentedJSON(http.StatusOK, task)
}

func showResult(c *gin.Context) {
	taskId := c.Param("id")

	resultPath := filepath.Join(rootDir, taskId, "result.json")
	// 读取文件
	fd, err := os.OpenFile(resultPath, os.O_RDONLY, 755)
	if err != nil {
		c.IndentedJSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	content, err := io.ReadAll(fd)
	res := results.AnsiblePlaybookJSONResults{}
	if err := json.Unmarshal(content, &res); err != nil {
		c.IndentedJSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	c.IndentedJSON(http.StatusOK, res)
}

func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func writeFile(path, content string) error {
	parentDir := filepath.Dir(path)
	if _, err := os.Stat(parentDir); os.IsNotExist(err) {
		os.MkdirAll(parentDir, 0755)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	if err != nil {
		return err
	}
	return nil
}

func updateTask(task Task) error {
	tx := db.Where("id = ?", task.ID).Updates(
		Task{
			Status:    task.Status,
			UpdatedAt: time.Now(),
		})
	if tx.Error != nil {
		return tx.Error
	}
	return nil
}

func startRunAnsiblePlaybookService(index int, wait *sync.WaitGroup) {
	defer func() {
		log.Printf("# %d service stopped\n", index)
		wait.Done()
	}()
	for {
		select {
		case taskId, ok := <-taskChan:
			if !ok {
				return
			}

			var task Task
			tx := db.Preload("Playbook").Preload("Inventory").Preload("User").First(&task, "task_id = ?", taskId)
			if tx.Error != nil {
				log.Printf("Error: task(%v) %v\n", taskId, tx.Error)
				continue
			}

			tx = db.Where("task_id = ?", taskId).Updates(Task{
				Status:    1,
				UpdatedAt: time.Now(),
			})
			if tx.Error != nil {
				log.Printf("Error: task(%v) %v\n", taskId, tx.Error)
				continue
			}

			err := runAnsiblePlaybook(&task)
			if err != nil {
				task.Status = 3
				task.Error = err.Error()
			} else {
				task.Status = 2
			}

			if err := updateTask(task); err != nil {
				log.Printf("Error: task(%v) %v\n", task, err)
				continue
			}

		}
	}
}

func runAnsiblePlaybook(task *Task) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(10)*time.Minute)
	defer cancel()

	buff := new(bytes.Buffer)
	cmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(task.Playbook.Path),
		playbook.WithPlaybookOptions(&playbook.AnsiblePlaybookOptions{
			Become: false,
			ExtraVars: map[string]interface{}{
				"gather_facts":                 false,
				"ansible_ssh_private_key_file": SSH_USER_PRI_KEY_FILE,
				"ansible_user":                 SSH_USER,
				"ansible_port":                 SSH_PORT,
			},
			Inventory:     task.Inventory.Path,
			SSHCommonArgs: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null",
		}),
	)
	exec := stdoutcallback.NewJSONStdoutCallbackExecute(
		execute.NewDefaultExecute(
			execute.WithCmd(cmd),
			execute.WithWrite(io.Writer(buff)),
		),
	)
	if err := exec.Execute(ctx); err != nil {
		return err
	}
	res, err := results.ParseJSONResultsStream(io.Reader(buff))
	if err != nil {
		return err
	}

	raw, err := json.MarshalIndent(res, "", "    ")
	if err != nil {
		return err
	}

	resultPath := filepath.Join(rootDir, task.TaskID, "result.json")
	if err := os.WriteFile(resultPath, raw, 0644); err != nil {
		return err
	}
	// for _, play := range res.Plays {
	// 	for _, task := range play.Tasks {
	// 		for hostname, item := range task.Hosts {
	// 			if item.Skipped {
	// 				continue
	// 			}
	// 			fmt.Println("+------+")
	// 			fmt.Println("> Host:", hostname)
	// 			fmt.Println("> Task:", task.Task.Name)
	// 			fmt.Println("> Cmd :", item.Cmd)
	// 			if item.Unreachable {
	// 				fmt.Println("> Stat: LOST")
	// 				continue
	// 			}
	// 			if item.Failed {
	// 				fmt.Println("> Stat:", item.Failed)
	// 				fmt.Println("> Err :", item.Stderr)
	// 				continue
	// 			}
	// 			fmt.Println("> Out :", item.Stdout)
	// 		}
	// 	}
	// }
	return nil
}
