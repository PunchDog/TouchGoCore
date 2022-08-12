// Copyright 2013 Ardan Studios. All rights reserved.
// Use of baseController source code is governed by a BSD-style
// license that can be found in the LICENSE handle.

// Package baseController implements boilerplate code for all baseControllers.
package beegoweb

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"touchgocore/vars"

	"github.com/astaxie/beego"
	"github.com/astaxie/beego/session"
	"github.com/astaxie/beego/validation"
)

//** TYPES

type GmResp struct {
	Value interface{} `json:"values"`
}
type CommonResp struct {
	Message int16 `json:"code"`
}

type Response map[string]interface{}

func (resp Response) Push(key string, val interface{}) {
	resp[key] = val
}

type (
	// BaseController composes all required types and behavior.
	BaseController struct {
		beego.Controller
		UserID string
	}
)

func (baseController *BaseController) GmResponse(response interface{}) {
	baseController.Data["json"] = response
	baseController.ServeJSON()
}

//** INTERCEPT FUNCTIONS

// Prepare is called prior to the baseController method.

func (baseController *BaseController) StartSession() (sess session.Store) {
	sess, _ = beego.GlobalSessions.SessionStart(baseController.Ctx.ResponseWriter, baseController.Ctx.Request)
	return

}

func (baseController *BaseController) NewResponse() Response {
	return make(Response)
}
func (baseController *BaseController) Prepare() {
	baseController.UserID = baseController.GetString("userID")
	if baseController.UserID == "" {
		baseController.UserID = baseController.GetString(":userID")
	}
	if baseController.UserID == "" {
		baseController.UserID = "Unknown"
	}

	baseController.Data["region"] = baseController.Ctx.Input.Header("Region-Id")

	//url := baseController.Ctx.Input.URL()
	//ip := baseController.Ctx.Input.IP()
	//	if authService.IsForbidCmd(url) && misc.IsTrustedIP(ip) == false {
	//		baseController.Ctx.Output.SetStatus(protocol.CMD_FORBIDED) // 527.禁用命令
	//		baseController.ServeJson()
	//		return
	//	}

	//	if err := baseController.Service.Prepare(); err != nil {
	//		baseController.ServeError(err)
	//		return
	//	}

}

// Finish is called once the baseController method completes.
func (baseController *BaseController) Finish() {
	defer func() {
		// if baseController.MongoSession != nil {
		// 	mongo.CloseSession(baseController.UserID, baseController.MongoSession)
		// 	baseController.MongoSession = nil
		// }
	}()
}

//** VALIDATION

// ParseAndValidate will run the params through the validation framework and then
// response with the specified localized or provided message.
func (baseController *BaseController) ParseAndValidate(params interface{}) bool {
	// This is not working anymore :(
	if err := baseController.ParseForm(params); err != nil {
		baseController.ServeError(err)
		return false
	}

	var valid validation.Validation
	ok, err := valid.Valid(params)
	if err != nil {
		baseController.ServeError(err)
		return false
	}

	if ok == false {
		// Build a map of the Error messages for each field
		messages2 := make(map[string]string)

		val := reflect.ValueOf(params).Elem()
		for i := 0; i < val.NumField(); i++ {
			// Look for an Error tag in the field
			typeField := val.Type().Field(i)
			tag := typeField.Tag
			tagValue := tag.Get("Error")

			// Was there an Error tag
			if tagValue != "" {
				messages2[typeField.Name] = tagValue
			}
		}

		// Build the Error response
		var errors []string
		for _, err := range valid.Errors {
			// Match an Error from the validation framework Errors
			// to a field name we have a mapping for

			// No match, so use the message as is
			errors = append(errors, err.Message)
		}

		baseController.ServeValidationErrors(errors)
		return false
	}

	return true
}

//** EXCEPTIONS

// ServeError prepares and serves an Error exception.
func (baseController *BaseController) ServeError(err error) {
	baseController.Data["json"] = struct {
		Error string `json:"Error"`
	}{err.Error()}
	baseController.Ctx.Output.SetStatus(500)
	baseController.ServeJSON()
}

// ServeError prepares and serves an Error exception.
func (baseController *BaseController) ServeRsp(err error) {
	baseController.Data["json"] = struct {
		Error string `json:"Error"`
	}{err.Error()}
	baseController.Ctx.Output.SetStatus(200)
	baseController.ServeJSON()
}

// ServeValidationErrors prepares and serves a validation exception.
func (baseController *BaseController) ServeValidationErrors(Errors []string) {
	baseController.Data["json"] = struct {
		Errors []string `json:"Errors"`
	}{Errors}
	baseController.Ctx.Output.SetStatus(409)
	baseController.ServeJSON()
}

//** CATCHING PANICS

// CatchPanic is used to catch any Panic and log exceptions. Returns a 500 as the response.
func (baseController *BaseController) CatchPanic(functionName string) {
	if r := recover(); r != nil {
		buf := make([]byte, 10000)
		runtime.Stack(buf, false)

		baseController.ServeError(fmt.Errorf("%v", r))
	}
}

//** AJAX SUPPORT

// AjaxResponse returns a standard ajax response.
func (baseController *BaseController) AjaxResponse(resultCode int, resultString string, data interface{}) {
	response := struct {
		Result       int
		ResultString string
		ResultObject interface{}
	}{
		Result:       resultCode,
		ResultString: resultString,
		ResultObject: data,
	}

	baseController.Data["json"] = response
	baseController.ServeJSON()
}

func (this *BaseController) Display(tpl string) {
	//this.Layout = "layout.html"
	this.TplName = tpl + ".html"
}

func (this *BaseController) FormatJson(data interface{}) string {
	jsonExpr := string("{}")
	if b, err := json.Marshal(data); err != nil {
		vars.Error("json.Marshal falied: %s", err)
	} else {
		jsonExpr = string(b)
	}
	return jsonExpr
}
