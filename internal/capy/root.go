package capy

import (
	"context"
	"fmt"
	"strings"
)

// CreateRootThreadLinked 在项目下创建根线程，工作区用「设备已链接的目录」（不建 worktree）。
func (s *Session) CreateRootThreadLinked(ctx context.Context, org, projectID, deviceID, root, title, message, modelID, reasoning string, files []FileRef) (*Jam, error) {
	jamID := "jam_" + NewULID()
	req := CreateJamRequest{
		JamID: jamID, RequestID: newUUID(), Title: title, Message: message,
		Model:  ModelSelection{ModelID: modelID, ReasoningMode: reasoning},
		Device: &Device{DeviceID: deviceID, Workspace: Workspace{Kind: "linked", Root: root}},
		Files:  files,
	}
	var jam Jam
	if err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/projects/%s/jams", org, projectID), org, req, &jam); err != nil {
		return nil, err
	}
	if jam.ID == "" {
		jam.ID = jamID
	}
	return &jam, nil
}

// CreateRootThread 在项目下创建一条「根线程」，并绑定到指定设备。
//
// 与子线程的区别：根线程没有父线程，不会继承别人的对话上下文/记忆；
// 代价是创建时设备必须在线（广告模型还要求线程来自 App/网页侧的会话）。
// 创建完成后即使设备离线，这条线程仍能继续对话（实测）。
func (s *Session) CreateRootThread(ctx context.Context, org, projectID, deviceID, workspaceRoot, title, message, modelID, reasoning string, files []FileRef) (*Jam, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("缺少项目 id")
	}
	jamID := "jam_" + NewULID()
	device := &Device{DeviceID: deviceID, Workspace: Workspace{Kind: "worktree", Root: workspaceRoot + "/" + jamID}}
	req := CreateJamRequest{
		JamID:     jamID,
		RequestID: newUUID(),
		Title:     title,
		Message:   message,
		Model:     ModelSelection{ModelID: modelID, ReasoningMode: reasoning},
		Device:    device,
		Files:     files,
	}
	var jam Jam
	if err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/projects/%s/jams", org, projectID), org, req, &jam); err != nil {
		return nil, err
	}
	if jam.ID == "" {
		jam.ID = jamID
	}
	return &jam, nil
}
