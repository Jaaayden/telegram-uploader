package telegram

import (
	"context"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/telegram/tljson"
	"github.com/gotd/td/tg"
)

func (c *Client) queryMaxUploadBytes(ctx context.Context) (int64, bool) {
	maxParts := int64(constant.UploadMaxParts)
	result, err := c.raw.API().HelpGetAppConfig(ctx, 0)
	if err != nil {
		return maxParts * int64(constant.UploadMaxPartSize), false
	}
	appConfig, ok := result.(*tg.HelpAppConfig)
	if !ok {
		return maxParts * int64(constant.UploadMaxPartSize), false
	}
	var decoded tljson.AppConfig
	if decoded.DecodeJSONValue(appConfig.Config) != nil {
		return maxParts * int64(constant.UploadMaxPartSize), false
	}
	value, ok := decoded.Unparsed["upload_max_fileparts_default"].(*tg.JSONNumber)
	if !ok || value.Value <= 0 {
		return maxParts * int64(constant.UploadMaxPartSize), false
	}
	serverParts := int64(value.Value)
	// gotd currently validates against its compiled protocol limit. Never
	// advertise a size which this transport would reject.
	if serverParts < maxParts {
		maxParts = serverParts
	}
	return maxParts * int64(constant.UploadMaxPartSize), true
}
