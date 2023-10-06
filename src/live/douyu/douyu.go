// 包 douyu 包含了与斗鱼直播平台相关的代码。
package douyu

import (
	// 导入所需的包和库
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/yuhaohwang/requests"

	"github.com/yuhaohwang/bililive-go/src/live"
	"github.com/yuhaohwang/bililive-go/src/live/internal"
	"github.com/yuhaohwang/bililive-go/src/pkg/utils"

	"github.com/robertkrimen/otto"
	uuid "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
)

// 常量定义
const (
	domain = "www.douyu.com"
	cnName = "斗鱼"

	liveInfoUrl = "https://www.douyu.com/betard"
	liveEncUrl  = "https://www.douyu.com/swf_api/homeH5Enc"
	liveAPIUrl  = "https://www.douyu.com/lapi/live/getH5Play"
)

// 初始化函数，注册斗鱼直播平台
func init() {
	live.Register(domain, new(builder))
}

// builder 结构体用于构建斗鱼直播实例
type builder struct{}

// Build 方法用于创建斗鱼直播实例
func (b *builder) Build(url *url.URL, opt ...live.Option) (live.Live, error) {
	return &Live{
		BaseLive: internal.NewBaseLive(url, opt...),
	}, nil
}

// cryptoJS 全局变量用于存储 CryptoJS 库的 JavaScript 代码
var cryptoJS []byte

// douyuRoomIDRegs 数组包含了不同的正则表达式，用于从页面中提取斗鱼房间ID
var douyuRoomIDRegs = []string{
	`\$ROOM\.room_id\s*=\s*(\d+)`,
	`room_id\s*=\s*(\d+)`,
	`"room_id.?":(\d+)`,
	`data-onlineid=(\d+)`,
}

// workflowReg 正则表达式用于匹配 JavaScript 中的工作流代码
var workflowReg = `function ub98484234\(.+?\Weval\((\w+)\);`

// jsDomTmpl 模板用于构建 JavaScript DOM 代码
var jsDomTmpl = template.Must(template.New("jsDom").Parse(`
	{{.DebugMessages}} = { {{.DecryptedCodes}}: []};
	if (!this.window) {window = {};}
	if (!this.document) {document = {};}
`))

// jsPatchTmpl 模板用于构建 JavaScript 补丁代码
var jsPatchTmpl = template.Must(template.New("jsPatch").Parse(`
	{{.DebugMessages}}.{{.DecryptedCodes}}.push({{.Workflow}});
	var patchCode = function(workflow) {
		var testVari = /(\w+)=(\w+)\([\w\+]+\);.*?(\w+)="\w+";/.exec(workflow);
		if (testVari && testVari[1] == testVari[2]) {
			{{.Workflow}} += testVari[1] + "[" + testVari[3] + "] = function() {return true;};";
		}
	};
	patchCode({{.Workflow}});
	var subWorkflow = /(?:\w+=)?eval\((\w+)\)/.exec({{.Workflow}});
	if (subWorkflow) {
		var subPatch = (
			"{{.DebugMessages}}.{{.DecryptedCodes}}.push('sub workflow: ' + subWorkflow);" +
			"patchCode(subWorkflow);"
		).replace(/subWorkflow/g, subWorkflow[1]) + subWorkflow[0];
		{{.Workflow}} = {{.Workflow}}.replace(subWorkflow[0], subPatch);
	}
	eval({{.Workflow}});
`))

// jsDebugTmpl 模板用于构建 JavaScript 调试代码
var jsDebugTmpl = template.Must(template.New("jsDebug").Parse(`
	var {{.Ub98484234}} = ub98484234;
	ub98484234 = function(p1, p2, p3) {
		try {
			var resoult = {{.Ub98484234}}(p1, p2, p3);
			{{.DebugMessages}}.{{.Resoult}} = resoult;
		} catch(e) {
			{{.DebugMessages}}.{{.Resoult}} = e.message;
		}
		return {{.DebugMessages}};
	};
`))

// render 函数用于渲染模板
func render(tmpl *template.Template, data interface{}) (string, error) {
	buf := bytes.NewBuffer(nil)
	if err := tmpl.Execute(buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// loadCryptoJS 函数用于加载 CryptoJS 库
func loadCryptoJS() {
	var (
		resp *requests.Response
		body []byte
		err  error
	)
	// 尝试从多个CDN地址加载 CryptoJS 库
	cdnUrls := [...]string{"https://cdnjs.cloudflare.com/ajax/libs/crypto-js/3.1.9-1/crypto-js.min.js",
		"https://cdn.jsdelivr.net/npm/crypto-js@3.1.9-1/crypto-js.min.js",
		"https://cdn.staticfile.org/crypto-js/3.1.9-1/crypto-js.min.js",
		"https://cdn.bootcdn.net/ajax/libs/crypto-js/3.1.9-1/crypto-js.min.js"}

	for _, url := range cdnUrls {
		resp, err = requests.Get(url)
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		body, err = resp.Bytes()
		if err != nil {
			continue
		}
		cryptoJS = body
		return
	}
	panic(fmt.Errorf("failed to load CryptoJS, please check network"))
}

// getEngineWithCryptoJS 函数获取带有 CryptoJS 库的 JavaScript 引擎
func getEngineWithCryptoJS() (*otto.Otto, error) {
	if cryptoJS == nil {
		loadCryptoJS()
	}
	engine := otto.New()
	if _, err := engine.Eval(cryptoJS); err != nil {
		return nil, err
	}
	return engine, nil
}

// Live 结构体表示一个斗鱼直播实例
type Live struct {
	internal.BaseLive
	roomID string
}

// fetchRoomID 方法从斗鱼直播页面中提取房间ID
func (l *Live) fetchRoomID() error {
	if l.roomID != "" {
		return nil
	}
	var body []byte
	resp, err := requests.Get(l.Url.String(), live.CommonUserAgent)
	if err != nil {
		return errors.New("request failed. error: " + err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("response code is " + strconv.Itoa(resp.StatusCode))
	}
	body, err = resp.Bytes()
	if err != nil {
		return errors.New("failed to read response body. error: " + err.Error())
	}
	// 使用正则表达式从页面中提取房间ID
	for _, reg := range douyuRoomIDRegs {
		if str := utils.Match1(reg, string(body)); str != "" {
			l.roomID = str
			return nil
		}
	}
	if strings.Contains(string(body), "该房间目前没有开放") {
		errorMessage := "房间未开放"
		return errors.New(errorMessage)
	}
	if strings.Contains(string(body), "您观看的房间已被关闭，请选择其他直播进行观看哦！") {
		errorMessage := "房间被关闭"
		return errors.New(errorMessage)
	}
	showedBodyMaxLength := 20
	bodyLen := len(body)
	if bodyLen < 20 {
		showedBodyMaxLength = bodyLen
	}
	errorMessage := "unexcepted error. body: " + string(body[:showedBodyMaxLength])
	if bodyLen > showedBodyMaxLength {
		errorMessage += "... "
	}
	return errors.New(errorMessage)
}

// GetInfo 方法获取斗鱼直播房间的信息，包括主播名称、房间名称和直播状态
func (l *Live) GetInfo() (info *live.Info, err error) {
	if err := l.fetchRoomID(); err != nil {
		if err.Error() == "房间未开放" {
			return nil, errors.New("room not exists, fetchRoomID failed")
		} else if err.Error() == "房间被关闭" {
			return &live.Info{
				Live:     l,
				HostName: "您观看的房间已被关闭",
				RoomName: "您观看的房间已被关闭",
				Status:   false,
			}, nil
		} else {
			return nil, err
		}
	}
	resp, err := requests.Get(fmt.Sprintf("%s/%s", liveInfoUrl, l.roomID), live.CommonUserAgent)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GetInfo() failed, response code: %d", resp.StatusCode)
	}
	body, err := resp.Bytes()
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}

	err = json.Unmarshal(body, &result)

	if err != nil {
		print(body)
	}

	info = &live.Info{
		Live:         l,
		HostName:     gjson.GetBytes(body, "room.owner_name").String(),
		RoomName:     gjson.GetBytes(body, "room.room_name").String(),
		Status:       gjson.GetBytes(body, "room.show_status").Int() == 1 && gjson.GetBytes(body, "room.videoLoop").Int() == 0,
		CustomLiveId: "douyu/" + l.roomID,
	}
	return info, nil
}

// getSignParams 方法获取签名参数，用于后续获取直播流媒体URL
func (l *Live) getSignParams() (map[string]string, error) {
	resp, err := requests.Get(liveEncUrl, live.CommonUserAgent, requests.Query("rids", l.roomID))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getSignParams() failed, response code: %d", resp.StatusCode)
	}
	body, err := resp.Bytes()
	if err != nil {
		return nil, err
	}

	jsEnc := gjson.GetBytes(body, "data.room"+l.roomID).String()

	workflow := utils.Match1(workflowReg, jsEnc)

	context := struct {
		DebugMessages  string
		DecryptedCodes string
		Resoult        string
		Ub98484234     string
		Workflow       string
	}{
		DebugMessages:  utils.GenRandomName(8),
		DecryptedCodes: utils.GenRandomName(8),
		Resoult:        utils.GenRandomName(8),
		Ub98484234:     utils.GenRandomName(8),
		Workflow:       workflow,
	}
	jsDom, err := render(jsDomTmpl, context)
	if err != nil {
		return nil, err
	}
	jsPatch, err := render(jsPatchTmpl, context)
	if err != nil {
		return nil, err
	}
	jsDebug, err := render(jsDebugTmpl, context)
	if err != nil {
		return nil, err
	}

	jsEnc = strings.ReplaceAll(jsEnc, fmt.Sprintf("eval(%s);", context.Workflow), jsPatch)
	engine, err := getEngineWithCryptoJS()
	if err != nil {
		return nil, err
	}
	if _, err := engine.Eval(jsDom); err != nil {
		return nil, err
	}
	if _, err := engine.Eval(jsEnc); err != nil {
		return nil, err
	}
	if _, err := engine.Eval(jsDebug); err != nil {
		return nil, err
	}
	did := strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")
	ts := time.Now()
	res, err := engine.Call("ub98484234", nil, l.roomID, did, ts.Unix())
	if err != nil {
		return nil, err
	}
	values := map[string]string{
		"cdn":  "",
		"iar":  "0",
		"ive":  "0",
		"rate": "0",
	}
	resoult, err := res.Object().Get(context.Resoult)
	if err != nil {
		return nil, err
	}
	for _, entry := range strings.Split(resoult.String(), "&") {
		if entry == "" {
			continue
		}
		strs := strings.SplitN(entry, "=", 2)
		values[strs[0]] = strs[1]
	}
	return values, nil
}

// GetStreamUrls 方法获取直播流媒体的URL
func (l *Live) GetStreamUrls() (us []*url.URL, err error) {
	if err := l.fetchRoomID(); err != nil {
		return nil, err
	}
	params, err := l.getSignParams()
	if err != nil {
		return nil, err
	}
	resp, err := requests.Post(
		fmt.Sprintf("%s/%s", liveAPIUrl, l.roomID),
		requests.Form(params),
		requests.Header("origin", "https://www.douyu.com"),
		requests.Referer(l.GetRawUrl()),
		live.CommonUserAgent,
	)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, live.ErrInternalError
	}
	body, err := resp.Bytes()
	if err != nil {
		return nil, err
	}
	if errorInt := gjson.GetBytes(body, "error").Int(); errorInt != 0 {
		return nil, fmt.Errorf("GetStreamUrls() failed, error: %d", errorInt)
	}
	return utils.GenUrls(
		fmt.Sprintf("%s/%s",
			gjson.GetBytes(body, "data.rtmp_url").String(),
			gjson.GetBytes(body, "data.rtmp_live").String(),
		),
	)
}

// GetPlatformCNName 方法获取斗鱼直播平台的中文名称
func (l *Live) GetPlatformCNName() string {
	return cnName
}