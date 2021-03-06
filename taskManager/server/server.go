package server

import (
	"golang.org/x/net/context"
	tmrpc "external/grpc/taskManagerRPC"
	smrpc "external/grpc/searchManagerRPC"
	"external/comm"
	"dataCenter/dataClient"
	"sync"
	"taskManager/rpc"
)

type TMServerConfig struct {
	Addr           string
	SMAddr         string
    TasksDataCenterConf dataClient.DataCenterDesc
	UsersDataCenterConf dataClient.DataCenterDesc
}

type TMServer struct {
	addr string
	tasksDataClient *dataClient.DataCenterClient
	usersDataClient *dataClient.DataCenterClient
	smClient smrpc.SearchManagerClient
	tasksLock sync.RWMutex
	tasks map[uint64]*comm.TaskInfo
	userTasks map[uint32][]*comm.TaskInfo
}

func New(conf *TMServerConfig) *TMServer {
	tasksdc := dataClient.New(&conf.TasksDataCenterConf)
	tasksdc.InitTasksData()
	usersdc := dataClient.New(&conf.UsersDataCenterConf)
	usersdc.InitUsersData()

	return &TMServer{
		addr: conf.Addr,
		tasksDataClient: tasksdc,
		usersDataClient: usersdc,

		smClient: rpc.InitSearchManagerClient(conf.SMAddr),

		tasks: make(map[uint64]*comm.TaskInfo),
		userTasks: make(map[uint32][]*comm.TaskInfo),
	}
}

func generateTaskRespComm(errCode int32) *tmrpc.TaskManagerRespComm {
	return &tmrpc.TaskManagerRespComm{
		ErrorCode: errCode,
		ErrorMsg: comm.GetErrMsg(errCode),
	}
}

func generateCreateTaskResp(errCode int32) *tmrpc.CreateTaskResp {
	return &tmrpc.CreateTaskResp{
		Comm: generateTaskRespComm(errCode),
	}
}

func (s *TMServer)searchResponsersAsync(srreq *smrpc.SearchResponsersReq) {
	resp, err := s.smClient.SearchResponsers(context.Background(), srreq)
	if err != nil || resp.Comm.ErrorCode != comm.RetOK {
		logger.Errorf("Task search responsers failed, err:%s, msg:%s, req:%v. SET TASK TO ERROR STATUS!", err, resp.Comm.ErrorMsg, srreq)
		derr := s.tasksDataClient.UpdateTaskStatusByTaskID(resp.Task.ID, comm.TasKStatusSearchResponserFailed)
		if derr != nil {
			logger.Errorf("[DataCenter]Update task status to SRF failed, err:%s, req:%v.", derr, srreq)
		}

		s.tasksLock.Lock()
		s.tasks[resp.Task.ID].Status = comm.TasKStatusSearchResponserFailed
		s.tasksLock.Unlock()
		return
	}

	if resp.Task.Responsers == nil || len(resp.Task.Responsers) == 0 {
		logger.Infof("Find no responsers task id : %lu.", resp.Task.ID)
		err = s.tasksDataClient.UpdateTaskStatusByTaskID(resp.Task.ID, comm.TasKStatusSearchResponserNone)
		if err != nil {
			logger.Errorf("[DataCenter]Update task status to wating SRN, err:%s, req:%v.", err, srreq)
		}

		s.tasksLock.Lock()
		s.tasks[resp.Task.ID].Status = comm.TasKStatusSearchResponserNone
		s.tasksLock.Unlock()
		return
	}

	err = s.tasksDataClient.UpdateTaskStatusByTaskID(resp.Task.ID, comm.TaskStatusWaitingAccept)
	if err != nil {
		logger.Errorf("[DataCenter]Update task status to wating accept failed, err:%s, req:%v.", err, srreq)
	}
	s.tasksLock.Lock()
	s.tasks[resp.Task.ID].Status = comm.TaskStatusWaitingAccept

	var ti *comm.TaskInfo = s.tasks[resp.Task.ID]
	for _, responser := range resp.Task.Responsers {
		if responser != ti.Desc.RequesterID {
			logger.Infof("add responser id %u, task id : %lu.", responser, resp.Task.ID)
			s.userTasks[responser] = append(s.userTasks[responser], ti)
		}
	}
	s.tasksLock.Unlock()

	return
}

func (s *TMServer)CreateTask(ctx context.Context, ctReq *tmrpc.CreateTaskReq) (*tmrpc.CreateTaskResp, error) {
	//生成任务记录并持久化
	logger.Infof("Create task！")
	ti := &comm.TaskInfo{
		Status: comm.TaskStatusCreating,
		Desc: ctReq.CreateInfo,
		Requester: ctReq.CreateInfo.RequesterID,
	}

	err := s.tasksDataClient.PutTaskInfo(ti)
	if err != nil {
		logger.Errorf("[DataClient]Put task info failed, err:%s, req:%v.\n", err, ctReq)
		resp := generateCreateTaskResp(comm.RetPutTaskInfoErr)
		return resp, nil
	}

	//缓存任务
	s.tasksLock.Lock()
	s.tasks[ti.ID] = ti
	s.userTasks[ti.Requester] = append(s.userTasks[ti.Requester], ti)
	s.tasksLock.Unlock()

	//异步搜索备选响应者并发出任务通知
	srreq := &smrpc.SearchResponsersReq{
		Task: ti,
	}

	go s.searchResponsersAsync(srreq)

	//返回用户任务创建结果
	resp := generateCreateTaskResp(comm.RetOK)
	return resp, nil
}

func (s *TMServer)QueryUserTasks(ctx context.Context, ctReq *tmrpc.QueryUserTasksReq) (*tmrpc.QueryUserTasksResp, error) {
	logger.Infof("Query Tasks user id : %u.", ctReq.UserID)

	taskInfoWithUsers := make([]*comm.TaskInfoWithUsers, 0)

	s.tasksLock.RLock()
	defer s.tasksLock.RUnlock()

	_, ok := s.userTasks[ctReq.UserID]
	if ok == false {
		logger.Infof("No tasks user id : %u.", ctReq.UserID)
		resp := &tmrpc.QueryUserTasksResp{
			Comm: generateTaskRespComm(comm.RetUserHasNoTask),
			Tasks: taskInfoWithUsers,
		}

		return resp, nil
	}

	logger.Infof("Find tasks user id : %u.", ctReq.UserID)

	var tiwu *comm.TaskInfoWithUsers
	for _, ti := range s.userTasks[ctReq.UserID] {
		tiwu = &comm.TaskInfoWithUsers{
			ID: ti.ID,
			Status: ti.Status,
			Desc: ti.Desc,
			FulfilStatus: ti.FulfilStatus,
			Responsers: make([]*comm.UserInfo, 0),
			ChosenResponser: make([]*comm.UserInfo, 0),
		}

		requester, err := s.usersDataClient.GetUserInfoByID(ti.Requester)
		if err != nil {
			logger.Errorf("[DataCenter][Query User tasks]Get User Info from dataCenter failed, err:%s, user_id:%d, taskid:%d.",
				err, ti.Requester, ti.ID)
		} else {
			tiwu.Requester = requester
		}

		if ti.Status == comm.TaskStatusProcessing || ti.Status == comm.TaskStatusFulfilled {
			for _, cr := range ti.ChosenResponser {
				chosenResponser, err := s.usersDataClient.GetUserInfoByID(cr)
				if err != nil {
					logger.Errorf("[DataCenter][Query User tasks]Get User Info from dataCenter failed, err:%s, user_id:%d, taskid:%d.",
						err, cr, ti.ID)
				} else {
					tiwu.ChosenResponser = append(tiwu.ChosenResponser, chosenResponser)
				}
			}
		}

		for _, r := range ti.Responsers {
			responser, err := s.usersDataClient.GetUserInfoByID(r)
			if err != nil {
				logger.Errorf("[DataCenter][Query User tasks]Get User Info from dataCenter failed, err:%s, user_id:%d, taskid:%d.",
					err, r, ti.ID)
			} else {
				tiwu.Responsers = append(tiwu.Responsers, responser)
			}
		}

		taskInfoWithUsers = append(taskInfoWithUsers, tiwu)
	}

	resp := &tmrpc.QueryUserTasksResp{
		Comm: generateTaskRespComm(comm.RetOK),
		Tasks: taskInfoWithUsers,
	}

	return resp, nil
}

func (s *TMServer)AcceptTask(ctx context.Context, atReq *tmrpc.AcceptTaskReq) (*tmrpc.AcceptTaskResp, error) {
	var resp *tmrpc.AcceptTaskResp

	s.tasksLock.Lock()

	if atReq.Decision == comm.TaskDecisionAccept {

		ti, ok := s.tasks[atReq.TaskID]
		if ok == false {
			resp = &tmrpc.AcceptTaskResp{
				Comm: generateTaskRespComm(comm.RetNoSuchTaks),
			}
		} else {
			ti.Responsers = append(ti.Responsers, atReq.ResponserID)
			ti.Status = comm.TaskStatusWaitingChoose

			err := s.tasksDataClient.UpdateTaskInfo(ti)
			if err != nil {
				logger.Errorf("[DataCenter][Accept Task]Update task info failed. err: %s, req: %v.", err, atReq)
				ti := s.tasks[atReq.TaskID]
				for index, r := range ti.Responsers {
					if r == atReq.ResponserID {
						ti.Responsers = append(ti.Responsers[:index], ti.Responsers[index+1:]...)
					}
				}

				if len(ti.Responsers) == 0 {
					ti.Status = comm.TaskStatusWaitingAccept
				}

				resp = &tmrpc.AcceptTaskResp{
					Comm: generateTaskRespComm(comm.RetUpdateTaskInfoErr),
				}
			} else {
				resp = &tmrpc.AcceptTaskResp{
					Comm: generateTaskRespComm(comm.RetOK),
				}
			}
		}
	} else {
		tis := s.userTasks[atReq.ResponserID]
		for index, ti := range tis {
			if atReq.TaskID == ti.ID {
				s.userTasks[atReq.ResponserID] = append(s.userTasks[atReq.ResponserID][:index], s.userTasks[atReq.ResponserID][index+1:]...)
			}
		}

		resp = &tmrpc.AcceptTaskResp{
			Comm: generateTaskRespComm(comm.RetOK),
		}
	}

	s.tasksLock.Unlock()

	return resp, nil
}

func (s *TMServer)ChooseTaskResponser(ctx context.Context, ctrReq *tmrpc.ChooseTaskResponserReq) (*tmrpc.ChooseTaskResponserResp, error) {
	var resp *tmrpc.ChooseTaskResponserResp

	s.tasksLock.Lock()

	ti := s.tasks[ctrReq.TaskID]
	ti.ChosenResponser = ctrReq.ChoseResponsersIDs
	ti.Status = comm.TaskStatusProcessing
	ti.FulfilStatus = make([]int32, len(ti.ChosenResponser))
	for i := 0; i < len(ti.FulfilStatus); i++ {
		ti.FulfilStatus[i] = comm.TaskFulfilStatusDoing
	}

	err := s.tasksDataClient.UpdateTaskInfo(ti)
	if err != nil {
		logger.Errorf("[DataCenter][Choose Task Responser]Update task info failed. err: %s, req: %v.", err, ctrReq)
		ti = s.tasks[ctrReq.TaskID]
		ti.ChosenResponser = nil
		ti.Status = comm.TaskStatusWaitingChoose

		resp = &tmrpc.ChooseTaskResponserResp{
			Comm: generateTaskRespComm(comm.RetUpdateTaskInfoErr),
		}
	} else {
		ti = s.tasks[ctrReq.TaskID]
		findTag := false
		for _, r := range ti.Responsers {
			for _, cr := range ti.ChosenResponser {
				if r == cr {
					findTag = true
					break
				}
			}

			if !findTag {
				for index, t := range s.userTasks[r] {
					if t.ID == ctrReq.TaskID {
						s.userTasks[r] = append(s.userTasks[r][:index], s.userTasks[r][index+1:]...)
					}
				}
			}

			findTag = false
		}

		resp = &tmrpc.ChooseTaskResponserResp{
			Comm: generateTaskRespComm(comm.RetOK),
		}
	}

	s.tasksLock.Unlock()

	return resp, nil
}

func (s *TMServer)FulfilTask(ctx context.Context, ftReq *tmrpc.FulfilTaskReq) (*tmrpc.FulfilTaskResp, error) {
	var resp *tmrpc.FulfilTaskResp

	s.tasksLock.Lock()

	ti := s.tasks[ftReq.TaskID]
	for index, cr := range ti.ChosenResponser {
		if cr == ftReq.ResponserID {
			ti.FulfilStatus[index] = comm.TaskFulfilStatusFinished
		}
	}

	taskFinishTag := true
	for _, s := range ti.FulfilStatus {
		if s != comm.TaskFulfilStatusFinished {
			taskFinishTag = false
			break
		}
	}

	if taskFinishTag {
		ti.Status = comm.TaskStatusFulfilled
	}

	err := s.tasksDataClient.UpdateTaskInfo(ti)
	if err != nil {
		logger.Errorf("[DataCenter][Fulfil Task]Update task info failed. err: %s, req: %v.", err, ftReq)
		ti.Status = comm.TaskStatusProcessing

		resp = &tmrpc.FulfilTaskResp{
			Comm: generateTaskRespComm(comm.RetUpdateTaskInfoErr),
		}
	} else {
		resp = &tmrpc.FulfilTaskResp{
			Comm: generateTaskRespComm(comm.RetOK),
		}
	}

	s.tasksLock.Unlock()

	return resp, nil
}

func (s *TMServer)EvaluateAndFinishTask(ctx context.Context, etReq *tmrpc.EvaluateAndFinishTaskReq) (*tmrpc.EvaluateAndFinishTaskResp, error) {

	s.tasksLock.Lock()

	ti := s.tasks[etReq.TaskID]
	for index, cti := range s.userTasks[ti.Requester] {
		if cti.ID == etReq.TaskID {
			s.userTasks[ti.Requester] = append(s.userTasks[ti.Requester][:index], s.userTasks[ti.Requester][index+1:]...)
			if len(s.userTasks[ti.Requester]) == 0 {
				delete(s.userTasks, ti.Requester)
			}
		}
	}

	for _, cr := range ti.ChosenResponser {
		for index, cti := range s.userTasks[cr] {
			if cti.ID == etReq.TaskID {
				s.userTasks[cr] = append(s.userTasks[cr][:index], s.userTasks[cr][index+1:]...)
				if len(s.userTasks[cr]) == 0 {
					delete(s.userTasks, cr)
				}
			}
		}
	}

	delete(s.tasks, etReq.TaskID)

	s.tasksLock.Unlock()

	resp := &tmrpc.EvaluateAndFinishTaskResp{
		Comm: generateTaskRespComm(comm.RetOK),
	}

	return resp, nil
}