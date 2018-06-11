package module

import (
	"github.com/Cyinx/einx/agent"
	"github.com/Cyinx/einx/component"
	"github.com/Cyinx/einx/event"
	"github.com/Cyinx/einx/slog"
	"github.com/Cyinx/einx/timer"
	"runtime"
	"sync"
	"time"
)

type Agent = agent.Agent
type AgentID = agent.AgentID
type AgentSessionMgr = agent.AgentSessionMgr
type Component = component.Component
type ComponentID = component.ComponentID
type ComponentMgr = component.ComponentMgr
type EventMsg = event.EventMsg
type EventType = event.EventType
type ComponentEventMsg = event.ComponentEventMsg
type SessionEventMsg = event.SessionEventMsg
type DataEventMsg = event.DataEventMsg
type RpcEventMsg = event.RpcEventMsg
type EventQueue = event.EventQueue
type TimerHandler = timer.TimerHandler
type TimerManager = timer.TimerManager
type ProtoTypeID = uint32
type ModuleID = uint32
type DispatchHandler func(event_msg EventMsg)
type MsgHandler func(Agent, interface{})
type RpcHandler func(interface{}, []interface{})

type Module interface {
	Close()
	GetName() string
	Run(*sync.WaitGroup)
	RpcCall(string, ...interface{})
	RouterAgentHandler(Agent, ProtoTypeID, interface{})
	RegisterHandler(ProtoTypeID, MsgHandler)
	RegisterRpcHandler(string, RpcHandler)
	AddTimer(delay uint64, op TimerHandler, args ...interface{}) uint64
	RemoveTimer(timer_id uint64)
}

type ModuleEventer interface {
	PostEvent(EventType, Agent, ComponentID)
	PostData(EventType, ProtoTypeID, Agent, interface{})
	PushEventMsg(ev EventMsg)
}

type module struct {
	id              ModuleID
	ev_queue        *EventQueue
	rpc_queue       *EventQueue
	name            string
	msg_handler_map map[ProtoTypeID]MsgHandler
	rpc_handler_map map[string]RpcHandler
	agent_map       map[AgentID]Agent
	commgr_map      map[ComponentID]interface{}
	component_map   map[ComponentID]Component
	rpc_msg_pool    *sync.Pool
	data_msg_pool   *sync.Pool
	event_msg_pool  *sync.Pool
	timer_manager   *TimerManager
	op_count        int64
	close_chan      chan bool
}

func (this *module) GetName() string {
	return this.name
}
func (this *module) PushEventMsg(ev EventMsg) {
	this.ev_queue.Push(ev)
}

func (this *module) Close() {
	this.close_chan <- true
	slog.LogWarning("module", "module [%s] will close!", this.name)
}

func (this *module) AddTimer(delay uint64, op TimerHandler, args ...interface{}) uint64 {
	return this.timer_manager.AddTimer(delay, op, args)
}

func (this *module) RemoveTimer(timer_id uint64) {
	this.timer_manager.DeleteTimer(timer_id)
}

func (this *module) PostEvent(event_type EventType, agent Agent, cid ComponentID) {
	event := this.event_msg_pool.Get().(*SessionEventMsg)
	event.MsgType = event_type
	event.Sender = agent
	event.Cid = cid
	this.ev_queue.Push(event)
}

func (this *module) PostData(event_type EventType, type_id ProtoTypeID, agent Agent, data interface{}) {
	event := this.data_msg_pool.Get().(*DataEventMsg)
	event.MsgType = event_type
	event.Sender = agent
	event.TypeID = type_id
	event.MsgData = data
	this.ev_queue.Push(event)
}

func (this *module) RpcCall(name string, args ...interface{}) {
	rpc_msg := this.rpc_msg_pool.Get().(*RpcEventMsg)
	rpc_msg.MsgType = event.EVENT_MODULE_RPC
	rpc_msg.Sender = this
	rpc_msg.Data = args
	rpc_msg.RpcName = name
	this.rpc_queue.Push(rpc_msg)
}

func (this *module) RouterAgentHandler(agent Agent, msg_id ProtoTypeID, msg interface{}) {
	this.PostData(event.EVENT_TCP_READ_MSG, msg_id, agent, msg)
}

func (this *module) RegisterHandler(type_id ProtoTypeID, handler MsgHandler) {
	_, ok := this.msg_handler_map[type_id]
	if ok == true {
		slog.LogWarning("module", "MsgID[%d] has been registered", type_id)
		return
	}
	this.msg_handler_map[type_id] = handler
}

func (this *module) RegisterRpcHandler(rpc_name string, handler RpcHandler) {
	_, ok := this.rpc_handler_map[rpc_name]
	if ok == true {
		slog.LogWarning("module", "Rpc[%s] has been registered", rpc_name)
		return
	}
	this.rpc_handler_map[rpc_name] = handler
}

func (this *module) Run(wait *sync.WaitGroup) {
	runtime.LockOSThread()
	wait.Add(1)
	ev_queue := this.ev_queue
	rpc_queue := this.rpc_queue
	rpc_chan := rpc_queue.GetChan()
	timer_manager := this.timer_manager
	var event_msg EventMsg = nil
	var event_count uint32 = 0
	var event_index uint32 = 0
	var event_chan = ev_queue.GetChan()
	var close_flag bool = false
	var ticker = time.NewTicker(15 * time.Millisecond)
	var now = time.Now().UnixNano() / 1e6
	event_list := make([]interface{}, 128)
	for {
		select {
		case close_flag = <-this.close_chan:
			if close_flag == true {
				goto run_close
			}
		case <-rpc_chan:
			event_count = rpc_queue.Get(event_list, 128)
			for event_index = 0; event_index < event_count; event_index++ {
				event_msg = event_list[event_index].(EventMsg)
				event_list[event_index] = nil
				this.handle_rpc(event_msg)
				this.op_count++
			}
		case <-event_chan:
			event_count = ev_queue.Get(event_list, 128)
			for event_index = 0; event_index < event_count; event_index++ {
				event_msg = event_list[event_index].(EventMsg)
				event_list[event_index] = nil
				this.handle_event(event_msg)
				this.op_count++
			}
		case <-ticker.C:
			timer_manager.Execute(100)
		}
	}
run_close:
	elasp_time := time.Now().UnixNano()/1e6 - now
	slog.LogError("perfomance", "total %s %d %d %d", this.name, elasp_time, this.op_count, this.op_count/elasp_time)
	this.do_close(wait)
}

func (this *module) do_close(wait *sync.WaitGroup) {
	for _, c := range this.component_map {
		c.Close()
	}
	for _, a := range this.agent_map {
		a.Close()
	}
	wait.Done()
}

func (this *module) handle_event(event_msg EventMsg) {
	switch event_msg.GetType() {
	case event.EVENT_TCP_READ_MSG:
		this.handle_data_event(event_msg)
	case event.EVENT_COMPONENT_CREATE:
		this.handle_component_event(event_msg)
	case event.EVENT_COMPONENT_ERROR:
		this.handle_component_error(event_msg)
	case event.EVENT_TCP_ACCEPTED:
		this.handle_agent_enter(event_msg)
	case event.EVENT_TCP_CONNECTED:
		this.handle_agent_enter(event_msg)
	case event.EVENT_TCP_CLOSED:
		this.handle_agent_closed(event_msg)
	default:
		slog.LogError("einx", "handle_event unknow event msg [%v]", event_msg.GetType())
	}
}

func (this *module) handle_data_event(event_msg EventMsg) {
	data_event := event_msg.(*DataEventMsg)
	handler, ok := this.msg_handler_map[data_event.TypeID]
	if ok == true {
		handler(data_event.Sender, data_event.MsgData)
	} else {
		slog.LogError("module", "module [%s] unregister msg handler msg type id[%d] %v!", this.name, data_event.TypeID, ok)
	}
	event_msg.Reset()
	this.data_msg_pool.Put(event_msg)
}

func (this *module) handle_component_event(event_msg EventMsg) {
	com_event := event_msg.(*ComponentEventMsg)
	c := com_event.Sender
	if _, ok := this.component_map[c.GetID()]; ok == true {
		slog.LogError("component", "module[%v] register component[%v]", this.name, c.GetID())
		return
	}

	this.component_map[c.GetID()] = c
	this.commgr_map[c.GetID()] = com_event.Attach
	mgr := com_event.Attach.(ComponentMgr)
	mgr.OnComponentCreate(c.GetID(), c)
}

func (this *module) handle_component_error(event_msg EventMsg) {
	com_event := event_msg.(*ComponentEventMsg)
	c := com_event.Sender
	if i_mgr, ok := this.commgr_map[c.GetID()]; ok == true {
		mgr := i_mgr.(ComponentMgr)
		mgr.OnComponentError(c, com_event.Attach.(error))
		return
	}
	slog.LogError("component", "module[%v] not register component[%v] manager cant handle error:%v", this.name, c.GetID(), com_event.Attach)
}

func (this *module) handle_agent_enter(event_msg EventMsg) {
	ses_event := event_msg.(*SessionEventMsg)
	agent := ses_event.Sender.(Agent)
	this.agent_map[agent.GetID()] = agent

	if sesmgr, ok := this.commgr_map[ses_event.Cid]; ok == true {
		sesmgr.(AgentSessionMgr).OnAgentEnter(agent.GetID(), agent)
		return
	}
	slog.LogError("agent", "module[%v] agent enter not found component[%v]", this.name, ses_event.Cid)
}

func (this *module) handle_agent_closed(event_msg EventMsg) {
	ses_event := event_msg.(*SessionEventMsg)
	agent := ses_event.Sender.(Agent)
	delete(this.agent_map, agent.GetID())
	if sesmgr, ok := this.commgr_map[ses_event.Cid]; ok == true {
		sesmgr.(AgentSessionMgr).OnAgentExit(agent.GetID(), agent)
		return
	}
	slog.LogError("agent", "module[%v] agent closed not found component[%v]", this.name, ses_event.Cid)
}

func (this *module) handle_rpc(event_msg EventMsg) {
	rpc_msg := event_msg.(*RpcEventMsg)
	if handler, ok := this.rpc_handler_map[rpc_msg.RpcName]; ok == true {
		handler(rpc_msg.Sender, rpc_msg.Data)
	} else {
		slog.LogError("module", "module [%v] unregister rpc handler! rpc name:[%v]", this.name, rpc_msg.RpcName)
	}
	event_msg.Reset()
	this.rpc_msg_pool.Put(rpc_msg)
	return
}
