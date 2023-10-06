package douyin

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yuhaohwang/requests"

	"github.com/yuhaohwang/bililive-go/src/live"
	"github.com/yuhaohwang/bililive-go/src/live/internal"
	"github.com/yuhaohwang/bililive-go/src/pkg/utils"
)

const (
	domain = "live.douyin.com"
	cnName = "抖音"

	randomCookieChars          = "1234567890abcdef"
	roomIdCatcherRegex         = `{\\"webrid\\":\\"([^"]+)\\"}`
	mainInfoLineCatcherRegex   = `self.__pace_f.push\(\[1,\s*"a:(\[.*\])\\n"\]\)`
	commonInfoLineCatcherRegex = `self.__pace_f.push\(\[1,\s*\"(\{.*\})\"\]\)`
)

var roomInfoApiForSprintf = "https://live.douyin.com/webcast/room/web/enter/?aid=6383&app_name=douyin_web&live_id=1&device_platform=web&language=zh-CN&browser_language=zh-CN&browser_platform=Win32&browser_name=Chrome&browser_version=116.0.0.0&web_rid=%s"

func init() {
	live.Register(domain, new(builder))
}

type builder struct{}

func (b *builder) Build(url *url.URL, opt ...live.Option) (live.Live, error) {
	return &Live{
		BaseLive:        internal.NewBaseLive(url, opt...),
		responseCookies: make(map[string]string),
		isUsingLegacy:   false,
	}, nil
}

func createRandomCookie() string {
	return utils.GenRandomString(21, randomCookieChars)
}

type Live struct {
	internal.BaseLive
	responseCookies             map[string]string
	LastAvailableStringUrlInfos []live.StreamUrlInfo
	isUsingLegacy               bool
}

// getLiveRoomWebPageResponse 获取直播房间网页响应
func (l *Live) getLiveRoomWebPageResponse() (body string, err error) {
	cookies := l.Options.Cookies.Cookies(l.Url)
	cookieKVs := make(map[string]string)
	cookieKVs["__ac_nonce"] = createRandomCookie()
	for key, value := range l.responseCookies {
		cookieKVs[key] = value
	}
	for _, item := range cookies {
		cookieKVs[item.Name] = item.Value
	}

	resp, err := requests.Get(
		l.Url.String(),
		live.CommonUserAgent,
		requests.Cookies(cookieKVs),
		requests.Headers(map[string]interface{}{
			"Cache-Control": "no-cache",
		}),
	)
	if err != nil {
		return
	}
	switch code := resp.StatusCode; code {
	case http.StatusOK:
		for _, cookie := range resp.Cookies() {
			l.responseCookies[cookie.Name] = cookie.Value
		}
	default:
		err = fmt.Errorf("获取网页失败，状态码：%v，%w", code, live.ErrInternalError)
		return
	}
	body, err = resp.Text()
	return
}

// getRoomInfoFromBody 从网页响应体中解析房间信息
func (l *Live) getRoomInfoFromBody(body string) (info *live.Info, streamUrlInfos []live.StreamUrlInfo, err error) {
	const errorMessageForErrorf = "getRoomInfoFromBody() 失败，步骤 %d"
	stepNumberForLog := 1
	mainInfoLine := utils.Match1(mainInfoLineCatcherRegex, body)
	if mainInfoLine == "" {
		err = fmt.Errorf(errorMessageForErrorf, stepNumberForLog)
		return
	}
	stepNumberForLog++

	// 获取房间信息
	mainJson := gjson.Parse(fmt.Sprintf(`"%s"`, mainInfoLine))
	if !mainJson.Exists() {
		err = fmt.Errorf(errorMessageForErrorf+". 无效的 JSON：%s", stepNumberForLog, mainInfoLine)
		return
	}

	mainJson = gjson.Parse(mainJson.String()).Get("3")
	if !mainJson.Exists() {
		err = fmt.Errorf(errorMessageForErrorf+". 主 JSON 不存在：%s", stepNumberForLog, mainInfoLine)
		return
	}

	isStreaming := mainJson.Get("state.roomStore.roomInfo.room.status_str").String() == "2"
	info = &live.Info{
		Live:     l,
		HostName: mainJson.Get("state.roomStore.roomInfo.anchor.nickname").String(),
		RoomName: mainJson.Get("state.roomStore.roomInfo.room.title").String(),
		Status:   isStreaming,
	}
	if !isStreaming {
		return
	}
	stepNumberForLog++

	// 获取直播流信息
	streamIdPath := "state.streamStore.streamData.H264_streamData.common.stream"
	streamId := mainJson.Get(streamIdPath).String()
	if streamId == "" {
		err = fmt.Errorf(errorMessageForErrorf+". %s 不存在：%s", stepNumberForLog, streamIdPath, mainInfoLine)
		return
	}
	stepNumberForLog++

	streamUrlInfos = make([]live.StreamUrlInfo, 0, 4)
	reg2, err := regexp.Compile(commonInfoLineCatcherRegex)
	if err != nil {
		return
	}
	match2 := reg2.FindAllStringSubmatch(body, -1)
	if match2 == nil {
		err = fmt.Errorf(errorMessageForErrorf, stepNumberForLog)
		return
	}
	stepNumberForLog++

	for _, item := range match2 {
		if len(item) < 2 {
			err = fmt.Errorf(errorMessageForErrorf+". len(item) = %d", stepNumberForLog, len(item))
			return
		}
		commonJson := gjson.Parse(gjson.Parse(fmt.Sprintf(`"%s"`, item[1])).String())
		if !commonJson.Exists() {
			err = fmt.Errorf(errorMessageForErrorf+". 无效的 JSON：%s", stepNumberForLog, item[1])
			return
		}
		commonStreamId := commonJson.Get("common.stream").String()
		if commonStreamId == "" {
			err = fmt.Errorf(errorMessageForErrorf+". 无效的 common stream ID：%s", stepNumberForLog, item[1])
			return
		}
		if commonStreamId != streamId {
			continue
		}

		commonJson.Get("data").ForEach(func(key, value gjson.Result) bool {
			flv := value.Get("main.flv").String()
			var Url *url.URL
			Url, err = url.Parse(flv)
			if err != nil {
				return true
			}
			paramsString := value.Get("main.sdk_params").String()
			paramsJson := gjson.Parse(paramsString)
			var description strings.Builder
			paramsJson.ForEach(func(key, value gjson.Result) bool {
				description.WriteString(key.String())
				description.WriteString(": ")
				description.WriteString(value.String())
				description.WriteString("\n")
				return true
			})
			Priority := 0
			resolution := strings.Split(paramsJson.Get("resolution").String(), "x")
			if len(resolution) == 2 {
				x, err := strconv.Atoi(resolution[0])
				if err != nil {
					return true
				}
				y, err := strconv.Atoi(resolution[1])
				if err != nil {
					return true
				}
				Priority = x * y
			}
			streamUrlInfos = append(streamUrlInfos, live.StreamUrlInfo{
				Name:        key.String(),
				Description: description.String(),
				Url:         Url,
				Priority:    Priority,
			})
			return true
		})
	}
	sort.Slice(streamUrlInfos, func(i, j int) bool {
		return streamUrlInfos[i].Priority > streamUrlInfos[j].Priority
	})
	stepNumberForLog++

	return
}

// GetInfo 获取直播信息
func (l *Live) GetInfo() (info *live.Info, err error) {
	l.isUsingLegacy = false
	body, err := l.getLiveRoomWebPageResponse()
	if err != nil {
		l.LastAvailableStringUrlInfos = nil
		return
	}

	var streamUrlInfos []live.StreamUrlInfo
	info, streamUrlInfos, err = l.getRoomInfoFromBody(body)
	if err == nil {
		l.LastAvailableStringUrlInfos = streamUrlInfos
		return
	}

	// 回退到使用 legacy 函数
	l.isUsingLegacy = true
	return l.legacy_GetInfo(body)
}

// GetStreamUrls 获取直播流媒体URL
func (l *Live) GetStreamUrls() (us []*url.URL, err error) {
	if !l.isUsingLegacy {
		if l.LastAvailableStringUrlInfos != nil {
			us = make([]*url.URL, 0, len(l.LastAvailableStringUrlInfos))
			for _, urlInfo := range l.LastAvailableStringUrlInfos {
				us = append(us, urlInfo.Url)
			}
			return
		}
		return nil, fmt.Errorf("获取抖音直播流媒体URL失败")
	} else {
		return l.legacy_GetStreamUrls()
	}
}

// GetPlatformCNName 获取直播平台的中文名称
func (l *Live) GetPlatformCNName() string {
	return cnName
}

// ================ legacy functions ================

// legacy_getRoomId 从网页响应体中解析房间ID
func (l *Live) legacy_getRoomId(body string) (string, error) {
	roomId := utils.Match1(roomIdCatcherRegex, body)
	if roomId == "" {
		return "", fmt.Errorf("无法从页面获取房间ID，%w", live.ErrInternalError)
	}
	return roomId, nil
}

// legacy_GetStreamUrls 获取直播流媒体URL（旧版）
func (l *Live) legacy_GetStreamUrls() (us []*url.URL, err error) {
	var body string
	body, err = l.getLiveRoomWebPageResponse()
	if err != nil {
		l.LastAvailableStringUrlInfos = nil
		return
	}
	data, err := l.legacy_getRoomInfo(body)
	if err != nil {
		return nil, err
	}
	var urls []string
	data.Get("data.0.stream_url.flv_pull_url").ForEach(func(key, value gjson.Result) bool {
		urls = append(urls, value.String())
		return true
	})
	streamData := gjson.Parse(data.Get("data.0.stream_url.live_core_sdk_data.pull_data.stream_data").String())
	if streamData.Exists() {
		url := streamData.Get("origin.main.flv")
		if url.Exists() {
			urls = append([]string{url.String()}, urls...)
		}
	}
	return utils.GenUrls(urls...)
}

// legacy_getRoomInfo 从网页响应体中解析房间信息（旧版）
func (l *Live) legacy_getRoomInfo(body string) (*gjson.Result, error) {
	roomId, err := l.legacy_getRoomId(body)
	if err != nil {
		return nil, err
	}

	cookies := l.Options.Cookies.Cookies(l.Url)
	cookieKVs := make(map[string]string)
	cookieKVs["__ac_nonce"] = createRandomCookie()
	for _, item := range cookies {
		cookieKVs[item.Name] = item.Value
	}
	for key, value := range l.responseCookies {
		cookieKVs[key] = value
	}
	roomInfoApi := fmt.Sprintf(roomInfoApiForSprintf, roomId)
	resp, err := requests.Get(
		roomInfoApi,
		live.CommonUserAgent,
		requests.Cookies(cookieKVs),
		requests.Headers(map[string]interface{}{
			"Cache-Control": "no-cache",
		}))
	if err != nil {
		return nil, err
	}
	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, live.ErrRoomNotExist
	default:
		return nil, fmt.Errorf("获取页面失败，状态码：%v，%w", code, live.ErrInternalError)
	}

	body, err = resp.Text()
	if err != nil {
		return nil, err
	}
	result := gjson.Parse(body)
	return &result, nil
}

// legacy_GetInfo 获取直播信息（旧版）
func (l *Live) legacy_GetInfo(body string) (info *live.Info, err error) {
	data, err := l.legacy_getRoomInfo(body)
	if err != nil {
		return nil, err
	}
	info = &live.Info{
		Live:     l,
		HostName: data.Get("user.nickname").String(),
		RoomName: data.Get("data.0.title").String(),
		Status:   data.Get("data.0.status").Int() == 2,
	}
	return
}
