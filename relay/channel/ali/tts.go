package ali

import (
	"bufio"
	"bytes"
	"encoding/base64"
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

type AliTTSOutput struct {
	Audio AliTTSAudio `json:"audio"`
}

type AliTTSAudio struct {
	URL  string `json:"url,omitempty"`
	Data string `json:"data,omitempty"`
	Type string `json:"type,omitempty"`
}

type AliTTSUsage struct {
	Characters   int `json:"characters"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func convertAudioRequest(_ *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	aliRequest := AliTTSRequest{
		Model: info.UpstreamModelName,
		Input: AliTTSInput{
			Text:  request.Input,
			Voice: request.Voice,
		},
	}

	if aliRequest.Input.Voice == "" {
		aliRequest.Input.Voice = "Cherry"
	}

	// Allow metadata to overlay custom fields (e.g. language_type, parameters)
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

	audioURL := aliResp.Output.Audio.URL
	if audioURL == "" {
		return types.NewOpenAIError(
			fmt.Errorf("no audio url in ali tts response"),
			types.ErrorCodeBadResponse, http.StatusBadRequest,
		), nil
	}

	// Download the audio from the URL
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
	if aliResp.Output.Audio.Type != "" {
		contentType = "audio/" + aliResp.Output.Audio.Type
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
	// Increase buffer size for base64 audio chunks
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

		// Write audio chunk
		if aliResp.Output.Audio.Data != "" {
			audioBytes, err := base64.StdEncoding.DecodeString(aliResp.Output.Audio.Data)
			if err != nil {
				logger.LogError(c, "ali tts stream base64 decode error: "+err.Error())
				continue
			}

			if !headerWritten {
				if aliResp.Output.Audio.Type != "" {
					contentType = "audio/" + aliResp.Output.Audio.Type
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

		// Extract usage from final event
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
