package ali

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// AliTTSRequest - DashScope TTS request format
// https://help.aliyun.com/zh/model-studio/text-to-speech
type AliTTSRequest struct {
	Model      string         `json:"model"`
	Input      AliTTSInput    `json:"input"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type AliTTSInput struct {
	Text         string `json:"text"`
	Voice        string `json:"voice,omitempty"`
	LanguageType string `json:"language_type,omitempty"`
}

// AliTTSResponse - DashScope TTS response (both streaming events and non-streaming)
type AliTTSResponse struct {
	RequestId string       `json:"request_id"`
	Output    AliTTSOutput `json:"output"`
	Usage     AliTTSUsage  `json:"usage"`
	AliError
}

// AliTTSOutput handles multiple DashScope response formats:
//   - Standard Ali (qwen-tts): output.audio = {"data":"base64...","type":"wav"}
//   - MiniMax format 1:        output.data  = {"audio":"hex-encoded-mp3"}
//   - MiniMax format 2:        output.audio = "hex-encoded-mp3"
type AliTTSOutput struct {
	AudioRaw json.RawMessage `json:"audio,omitempty"`
	DataRaw  json.RawMessage `json:"data,omitempty"`
}

type AliTTSAudio struct {
	URL  string `json:"url,omitempty"`
	Data string `json:"data,omitempty"`
	Type string `json:"type,omitempty"`
}

func (o *AliTTSOutput) extractAudioBytes() ([]byte, string) {
	// Standard Ali: output.audio is object {"data":"base64...","type":"..."}
	if len(o.AudioRaw) > 0 && o.AudioRaw[0] == '{' {
		var audioObj AliTTSAudio
		if err := json.Unmarshal(o.AudioRaw, &audioObj); err == nil {
			if audioObj.URL != "" {
				return nil, audioObj.Type
			}
			if audioObj.Data != "" {
				decoded, err := base64.StdEncoding.DecodeString(audioObj.Data)
				if err == nil {
					return decoded, audioObj.Type
				}
			}
		}
	}

	// MiniMax format 2: output.audio is a hex string
	if len(o.AudioRaw) > 0 && o.AudioRaw[0] == '"' {
		var audioStr string
		if err := json.Unmarshal(o.AudioRaw, &audioStr); err == nil && audioStr != "" {
			decoded, err := hex.DecodeString(audioStr)
			if err == nil && len(decoded) > 0 {
				return decoded, "mpeg"
			}
		}
	}

	// MiniMax format 1: output.data = {"audio":"hex..."}
	if len(o.DataRaw) > 0 && o.DataRaw[0] == '{' {
		var dataObj struct {
			Audio string `json:"audio"`
		}
		if err := json.Unmarshal(o.DataRaw, &dataObj); err == nil && dataObj.Audio != "" {
			decoded, err := hex.DecodeString(dataObj.Audio)
			if err == nil && len(decoded) > 0 {
				return decoded, "mpeg"
			}
		}
	}

	return nil, ""
}

func (o *AliTTSOutput) extractAudioURL() string {
	if len(o.AudioRaw) > 0 && o.AudioRaw[0] == '{' {
		var audioObj AliTTSAudio
		if err := json.Unmarshal(o.AudioRaw, &audioObj); err == nil {
			return audioObj.URL
		}
	}
	return ""
}

func (o *AliTTSOutput) extractAudioType() string {
	if len(o.AudioRaw) > 0 && o.AudioRaw[0] == '{' {
		var audioObj AliTTSAudio
		if err := json.Unmarshal(o.AudioRaw, &audioObj); err == nil {
			return audioObj.Type
		}
	}
	return ""
}

type AliTTSUsage struct {
	Characters   int `json:"characters"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func isMiniMaxModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "minimax") || strings.HasPrefix(lower, "speech-")
}

func convertAudioRequest(_ *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	modelName := info.UpstreamModelName

	if isMiniMaxModel(modelName) {
		return convertMiniMaxAudioRequest(modelName, request)
	}

	aliRequest := AliTTSRequest{
		Model: modelName,
		Input: AliTTSInput{
			Text:  request.Input,
			Voice: request.Voice,
		},
	}

	if aliRequest.Input.Voice == "" {
		aliRequest.Input.Voice = "Cherry"
	}

	if len(request.Metadata) > 0 {
		if err := common.Unmarshal(request.Metadata, &aliRequest); err != nil {
			return nil, fmt.Errorf("error unmarshalling metadata to ali tts request: %w", err)
		}
	}

	jsonData, err := common.Marshal(aliRequest)
	if err != nil {
		return nil, fmt.Errorf("error marshalling ali tts request: %w", err)
	}

	return bytes.NewReader(jsonData), nil
}

func convertMiniMaxAudioRequest(model string, request dto.AudioRequest) (io.Reader, error) {
	voice := request.Voice
	if voice == "" {
		voice = "female-shaonv"
	}

	speed := 1.0
	if request.Speed != nil {
		speed = *request.Speed
	}

	reqBody := map[string]interface{}{
		"model": model,
		"input": map[string]interface{}{
			"text": request.Input,
			"voice_setting": map[string]interface{}{
				"voice_id": voice,
				"speed":    speed,
				"vol":      1.0,
				"pitch":    0,
			},
			"audio_setting": map[string]interface{}{
				"sample_rate": 24000,
				"format":      "mp3",
				"channel":     1,
			},
		},
	}

	jsonData, err := common.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshalling minimax tts request: %w", err)
	}

	return bytes.NewReader(jsonData), nil
}

func buildTTSUsage(aliUsage AliTTSUsage) *dto.Usage {
	usage := &dto.Usage{}
	if aliUsage.Characters > 0 {
		// qwen3-tts-flash uses characters for billing
		usage.PromptTokens = aliUsage.Characters
		usage.TotalTokens = aliUsage.Characters
	} else {
		usage.PromptTokens = aliUsage.InputTokens
		usage.CompletionTokens = aliUsage.OutputTokens
		usage.TotalTokens = aliUsage.TotalTokens
	}
	return usage
}

func handleTTSResponse(c *gin.Context, resp *http.Response, _ *relaycommon.RelayInfo) (*types.NewAPIError, *dto.Usage) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError), nil
	}
	service.CloseResponseBodyGracefully(resp)

	var aliResp AliTTSResponse
	if err := common.Unmarshal(body, &aliResp); err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError), nil
	}

	if aliResp.Code != "" {
		return types.NewOpenAIError(
			fmt.Errorf("ali tts error: %s - %s", aliResp.Code, aliResp.Message),
			types.ErrorCodeBadResponse, http.StatusBadRequest,
		), nil
	}

	// Try inline audio first (MiniMax returns audio data directly)
	audioBytes, audioType := aliResp.Output.extractAudioBytes()
	if audioBytes != nil && len(audioBytes) > 0 {
		contentType := "audio/mpeg"
		if audioType != "" {
			contentType = "audio/" + audioType
		}
		c.Data(http.StatusOK, contentType, audioBytes)
		return nil, buildTTSUsage(aliResp.Usage)
	}

	// Standard Ali: download from URL
	audioURL := aliResp.Output.extractAudioURL()
	if audioURL == "" {
		return types.NewOpenAIError(
			fmt.Errorf("no audio data or url in ali tts response"),
			types.ErrorCodeBadResponse, http.StatusBadRequest,
		), nil
	}

	downloadResp, err := service.DoDownloadRequest(audioURL, "ali tts audio download")
	if err != nil {
		return types.NewOpenAIError(
			fmt.Errorf("failed to download ali tts audio: %w", err),
			types.ErrorCodeBadResponse, http.StatusInternalServerError,
		), nil
	}
	defer downloadResp.Body.Close()

	audioData, err := io.ReadAll(downloadResp.Body)
	if err != nil {
		return types.NewOpenAIError(
			fmt.Errorf("failed to read ali tts audio: %w", err),
			types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError,
		), nil
	}

	contentType := "audio/mpeg"
	if t := aliResp.Output.extractAudioType(); t != "" {
		contentType = "audio/" + t
	}
	c.Data(http.StatusOK, contentType, audioData)

	return nil, buildTTSUsage(aliResp.Usage)
}

func handleTTSStreamResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.Usage) {
	defer service.CloseResponseBodyGracefully(resp)

	var usage *dto.Usage
	contentType := "audio/mpeg"
	headerWritten := false

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimPrefix(line, "data:")

		var aliResp AliTTSResponse
		if err := common.Unmarshal([]byte(data), &aliResp); err != nil {
			logger.LogError(c, "ali tts stream unmarshal error: "+err.Error())
			continue
		}

		if aliResp.Code != "" {
			return types.NewOpenAIError(
				fmt.Errorf("ali tts stream error: %s - %s", aliResp.Code, aliResp.Message),
				types.ErrorCodeBadResponse, http.StatusBadRequest,
			), nil
		}

		audioBytes, audioType := aliResp.Output.extractAudioBytes()
		if audioBytes != nil && len(audioBytes) > 0 {
			if !headerWritten {
				if audioType != "" {
					contentType = "audio/" + audioType
				}
				c.Header("Content-Type", contentType)
				c.Status(http.StatusOK)
				headerWritten = true
			}

			if _, err := c.Writer.Write(audioBytes); err != nil {
				logger.LogError(c, "ali tts stream write error: "+err.Error())
				break
			}
			c.Writer.Flush()
		}

		if aliResp.Usage.Characters > 0 || aliResp.Usage.InputTokens > 0 || aliResp.Usage.TotalTokens > 0 {
			usage = buildTTSUsage(aliResp.Usage)
		}
	}

	if err := scanner.Err(); err != nil {
		logger.LogError(c, "ali tts stream scanner error: "+err.Error())
	}

	if usage == nil {
		usage = &dto.Usage{
			PromptTokens: info.GetEstimatePromptTokens(),
			TotalTokens:  info.GetEstimatePromptTokens(),
		}
	}

	return nil, usage
}
