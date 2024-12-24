package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/make-money-fast/xconfig"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var buildMap = sync.Map{}
var config Config
var lastBuildTimeMap = sync.Map{}
var lastDeployTimeMap = sync.Map{}

var mu sync.RWMutex
var conns = make(map[string][]*websocket.Conn)

type Config struct {
	Services []Services `json:"services"`
}
type Services struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	BuildScript    string `json:"buildScript"`
	DeployScript   string `json:"deployScript"`
	Status         string `json:"-"`
	Path           string `json:"path"`
	LastBuild      string `json:"lastBuild"`
	LastDeploy     string `json:"lastDeploy"`
	LastBuildHash  string `json:"lastBuildHash"`
	LastDeployHash string `json:"lastDeployHash"`
}

var configFile string

func init() {
	flag.StringVar(&configFile, "c", "config.yaml", "配置文件")
}

func main() {
	flag.Parse()
	if err := xconfig.ParseFromFile(configFile, &config); err != nil {
		panic(err)
	}

	g := gin.Default()
	g.LoadHTMLGlob("web/*.gohtml")

	g.GET("/", func(ctx *gin.Context) {
		svc := make([]*Services, 0, len(config.Services))
		for _, s := range config.Services {
			status, ok := buildMap.Load(s.Id)
			if !ok {
				status = "未构建"
			}
			x := &Services{
				Id:           s.Id,
				Name:         s.Name,
				BuildScript:  s.BuildScript,
				DeployScript: s.DeployScript,
				Status:       status.(string),
			}
			buildTime, ok := lastBuildTimeMap.Load(s.Id)
			if ok {
				x.LastBuild = buildTime.(time.Time).Format("2006-01-02 15:04:05")
			}
			deployTime, ok := lastDeployTimeMap.Load(s.Id)
			if ok {
				x.LastDeploy = deployTime.(time.Time).Format("2006-01-02 15:04:05")
			}
			svc = append(svc, x)
		}

		ctx.HTML(200, "index.gohtml", gin.H{
			"services": svc,
		})
	})

	g.GET("/build", func(ctx *gin.Context) {
		id := ctx.Query("id")
		service, ok := findById(id)
		if !ok {
			ctx.Redirect(302, "/")
			return
		}
		go build(service)
		ctx.Redirect(302, "/")
	})
	g.GET("/deploy", func(ctx *gin.Context) {
		id := ctx.Query("id")
		service, ok := findById(id)
		if !ok {
			ctx.Redirect(302, "/")
			return
		}
		go deploy(service)
		ctx.Redirect(302, "/")
	})

	g.GET("/logs", func(ctx *gin.Context) {
		id := ctx.Query("id")
		typ := ctx.Query("typ")
		ctx.HTML(200, "log.gohtml", gin.H{
			"id":  id,
			"typ": typ,
		})
	})

	type Release struct {
		Repo string `json:"repo"`
		Tag  string `json:"tag"`
	}

	g.POST("/release", func(ctx *gin.Context) {
		var req Release
		if err := ctx.ShouldBind(&req); err != nil {
			ctx.AbortWithError(400, err)
			return
		}
		if req.Repo == "" {
			ctx.AbortWithError(400, errors.New("repo is empty"))
			return
		}
		fmt.Println("receive hook: ", req.Repo, req.Tag)
		for _, s := range config.Services {
			if s.Name == req.Repo {
				fmt.Println("build and deploy by github hook")
				go build(&s)
			}
		}
		ctx.String(200, "ok")
	})

	g.Any("/githook", func(ctx *gin.Context) {
		var req GithubHook
		if err := ctx.ShouldBind(&req); err != nil {
			ctx.AbortWithError(400, err)
			return
		}

		fmt.Println("receive hook: ", req.Repository.Name, req.Ref)
		ref := req.Ref
		if !isMaster(ref) {
			ctx.String(200, "ok")
			return
		}

		repoName := req.Repository.Name

		for _, s := range config.Services {
			if s.Name == repoName {
				fmt.Println("build and deploy by github hook")
				go build(&s)
			}
		}

		ctx.String(200, "ok")
	})

	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // 允许所有来源（在生产环境中应更严谨）
		},
	}

	g.GET("/ws", func(ctx *gin.Context) {
		conn, err := upgrader.Upgrade(ctx.Writer, ctx.Request, nil)
		if err != nil {
			fmt.Println(err)
			return
		}
		id := ctx.Query("id")
		typ := ctx.Query("typ")
		mu.Lock()
		if _, ok := conns[id]; !ok {
			conns[id] = make([]*websocket.Conn, 0)
		}
		conns[id] = append(conns[id], conn)
		mu.Unlock()
		if err != nil {
			fmt.Println(err)
			return
		}
		// push already flushed logs
		logs := filepath.Join("logs", fmt.Sprintf("%s.%s.log", id, typ))
		data, err := ioutil.ReadFile(logs)
		if err == nil {
			// read multiples lines and send to client.
			for _, line := range strings.Split(string(data), "\n") {
				conn.WriteMessage(websocket.TextMessage, []byte(line))
			}
		}
		go func() {
			defer func() {
				mu.Lock()
				for idx, c := range conns[id] {
					if c == conn {
						conns[id][idx] = nil
						conns[id] = append(conns[id][:idx], conns[id][idx+1:]...)
					}
				}
				mu.Unlock()
				conn.Close()
			}()
			for {
				conn.SetReadDeadline(time.Now().Add(1 * time.Minute))
				_, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
			}
		}()
	})

	g.Run(":8083")
}

func broadCast(id string, message string) {
	mu.RLock()
	defer mu.RUnlock()
	cns := conns[id]
	if len(cns) == 0 {
		return
	}
	for _, conn := range cns {
		conn.WriteMessage(websocket.TextMessage, []byte(message))
	}
}

func findById(id string) (*Services, bool) {
	for _, s := range config.Services {
		if id == s.Id {
			return &s, true

		}
	}
	return nil, false
}

func build(s *Services) {
	defer func() {
		recover()
	}()
	_, ok := buildMap.Load(s.Id)
	if ok {
		return
	}
	buildMap.Store(s.Id, "构建中")

	env := os.Environ() // 获取当前的环境变量
	cmd := exec.Command("bash", "-c", s.BuildScript)
	cmd.Env = env // 设置命令的环境变量
	cmd.Dir = s.Path
	// 获取标准输出和标准错误的管道
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("获取标准输出管道失败:", err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Println("获取标准错误管道失败:", err)
		return
	}

	os.MkdirAll("logs", 0755)
	path := filepath.Join("logs", s.Id+".build.log")
	fi, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer fi.Close()

	// 使用 goroutine 读取标准输出
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				break
			}
			broadCast(s.Id, string(buf[:n]))
			fi.Write(buf[:n])
		}
	}()

	// 读取标准错误
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			broadCast(s.Id, string(buf[:n]))
			fi.Write(buf[:n])
		}
	}()
	// 启动命令
	if err := cmd.Start(); err != nil {
		broadCast(s.Id, fmt.Sprintf("命令启动失败: %s", err.Error()))
		fi.Write([]byte(fmt.Sprintf("命令启动失败: " + err.Error())))
		return
	}

	err = cmd.Wait()
	buildMap.Delete(s.Id)
	if err != nil {
		broadCast(s.Id, fmt.Sprintf("执行命令失败: %s", err.Error()))
		return
	}

	lastBuildTimeMap.Store(s.Id, time.Now())

	deploy(s)
}

func deploy(s *Services) {
	status, ok := buildMap.Load(s.Id)
	if ok {
		str := status.(string)
		if str == "部署中" {
			return
		}
	}
	buildMap.Store(s.Id, "部署中")
	defer func() {
		buildMap.Delete(s.Id)
	}()

	cmd := exec.Command("bash", "-c", s.DeployScript)
	cmd.Dir = s.Path
	// 获取标准输出和标准错误的管道
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("获取标准输出管道失败:", err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Println("获取标准错误管道失败:", err)
		return
	}

	os.MkdirAll("logs", 0755)
	path := filepath.Join("logs", s.Id+".deploy.log")
	fi, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer fi.Close()

	// 使用 goroutine 读取标准输出
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				break
			}
			broadCast(s.Id, string(buf[:n]))
			fi.Write(buf[:n])
		}
	}()

	// 读取标准错误
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			broadCast(s.Id, string(buf[:n]))
			fi.Write(buf[:n])
		}
	}()
	// 启动命令
	if err := cmd.Start(); err != nil {
		broadCast(s.Id, fmt.Sprintf("命令启动失败: %s", err.Error()))
		fi.Write([]byte(fmt.Sprintf("命令启动失败: " + err.Error())))
		return
	}

	if err := cmd.Wait(); err != nil {
		broadCast(s.Id, fmt.Sprintf("执行命令失败: %s", err.Error()))
	}
	lastDeployTimeMap.Store(s.Id, time.Now())
}

type GithubHook struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Repository struct {
		Id          int         `json:"id"`
		NodeId      string      `json:"node_id"`
		Name        string      `json:"name"`
		FullName    string      `json:"full_name"`
		Private     bool        `json:"private"`
		HtmlUrl     string      `json:"html_url"`
		Description interface{} `json:"description"`
	} `json:"repository"`
	Pusher struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"pusher"`
	Sender struct {
		Login             string `json:"login"`
		Id                int    `json:"id"`
		NodeId            string `json:"node_id"`
		AvatarUrl         string `json:"avatar_url"`
		GravatarId        string `json:"gravatar_id"`
		Url               string `json:"url"`
		HtmlUrl           string `json:"html_url"`
		FollowersUrl      string `json:"followers_url"`
		FollowingUrl      string `json:"following_url"`
		GistsUrl          string `json:"gists_url"`
		StarredUrl        string `json:"starred_url"`
		SubscriptionsUrl  string `json:"subscriptions_url"`
		OrganizationsUrl  string `json:"organizations_url"`
		ReposUrl          string `json:"repos_url"`
		EventsUrl         string `json:"events_url"`
		ReceivedEventsUrl string `json:"received_events_url"`
		Type              string `json:"type"`
		UserViewType      string `json:"user_view_type"`
		SiteAdmin         bool   `json:"site_admin"`
	} `json:"sender"`
	Created bool        `json:"created"`
	Deleted bool        `json:"deleted"`
	Forced  bool        `json:"forced"`
	BaseRef interface{} `json:"base_ref"`
	Compare string      `json:"compare"`
	Commits []struct {
		Id        string    `json:"id"`
		TreeId    string    `json:"tree_id"`
		Distinct  bool      `json:"distinct"`
		Message   string    `json:"message"`
		Timestamp time.Time `json:"timestamp"`
		Url       string    `json:"url"`
		Author    struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"committer"`
		Added    []interface{} `json:"added"`
		Removed  []interface{} `json:"removed"`
		Modified []string      `json:"modified"`
	} `json:"commits"`
	HeadCommit struct {
		Id        string    `json:"id"`
		TreeId    string    `json:"tree_id"`
		Distinct  bool      `json:"distinct"`
		Message   string    `json:"message"`
		Timestamp time.Time `json:"timestamp"`
		Url       string    `json:"url"`
		Author    struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"committer"`
		Added    []interface{} `json:"added"`
		Removed  []interface{} `json:"removed"`
		Modified []string      `json:"modified"`
	} `json:"head_commit"`
}

func isMaster(ref string) bool {
	return ref == "refs/heads/main" || ref == "refs/heads/master"
}
