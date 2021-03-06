package confgo

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/douyu/juno/pkg/service/parse"
	"github.com/douyu/jupiter/pkg/conf"

	"github.com/douyu/juno/pkg/model/view"

	"github.com/douyu/juno/pkg/model"
	"github.com/douyu/juno/pkg/model/db"
	"github.com/douyu/juno/pkg/service/codec"
	"github.com/douyu/juno/pkg/service/grpcgovern"
	"github.com/douyu/juno/pkg/service/resource"
	"github.com/douyu/jupiter/pkg/store/gorm"
	log "github.com/sirupsen/logrus"
)

type cmc struct{}

type confu struct {
	DB *gorm.DB
}

// 获取某个应用在某个环境的kv对
func (cmc *confu) GetAppKVs(caid int, itemId int) ([]db.CmcConfig, error) {
	dbConn := cmc.DB.Table("cmc_config")
	// var res = make(map[string]model.ConfigVal)
	var res []db.CmcConfig
	// 过滤删除的
	dbConn = dbConn.Where("`caid` = ? AND `id` <>? AND `status` <> ?", caid, itemId, model.ItemStatusDel).Order("`id` asc", false)
	err := dbConn.Find(&res).Error
	if err != nil {
		log.Warn("get cmc resources fail", "error", err)
		return res, err
	}

	return res, nil
}

// 添加配置
func (c *confu) Add(caid int, key, value string, resourceID int, opName string) (err error) {
	tx := c.DB.Begin()
	if err = c.AddWithTx(caid, key, value, resourceID, opName, tx); err != nil {
		tx.Rollback()
		return
	}
	tx.Commit()
	return
}

// 更新配置
func (c *confu) Update(id uint64, caid int, key, value string, resourceID int, opName string) (err error) {
	tx := c.DB.Begin()
	if err = c.UpdateWithTx(id, caid, key, value, resourceID, opName, tx); err != nil {
		tx.Rollback()
		return
	}
	tx.Commit()
	return
}

func (cmc *confu) GetDiffKeys(oriCid, rafCid int) (diffKeys []diffKeyItem, err error) {
	dbHandler := cmc.DB.Table("cmc_config")
	orikeys := make([]db.CmcConfig, 0)
	rafkeys := make([]db.CmcConfig, 0)
	if err = dbHandler.Where("caid = ? and status != 4", oriCid).Find(&orikeys).Error; err != nil {
		return
	}
	if err = dbHandler.Where("caid = ? and status != 4", rafCid).Find(&rafkeys).Error; err != nil {
		return
	}
	diffKeys = make([]diffKeyItem, 0)
	orimap := make(map[string]bool)
	rafmap := make(map[string]bool)
	for _, i := range orikeys {
		orimap[i.Key] = true
	}
	for _, i := range rafkeys {
		rafmap[i.Key] = true
	}
	for k, _ := range orimap {
		// 参考文本没有，源文件有
		if _, ok := rafmap[k]; !ok {
			diffKeys = append(diffKeys, diffKeyItem{Key: k, Status: "new"})
		}
	}
	for k, _ := range rafmap {
		// 参考文本有，源文件没有
		if _, ok := orimap[k]; !ok {
			diffKeys = append(diffKeys, diffKeyItem{Key: k, Status: "lack"})
		}
	}
	return
}

func (cmc *confu) GetAppKVlist(c *db.CmcConfig) ([]db.ConfigData, error) {
	dbConn := cmc.DB.Table("cmc_config")

	var res = make([]db.ConfigData, 0)
	var vals []db.CmcConfig
	if c.Key != "" {
		searchKey := "%" + c.Key + "%"
		dbConn = dbConn.Where("`key` like ?", searchKey)
	}
	dbConn = dbConn.Where("`caid` = ? ", c.Caid)
	err := dbConn.Find(&vals).Error
	if err != nil {
		log.Warn("get cmc resources fail", "error", err)
		return res, err
	}
	for _, v := range vals {
		val := db.ConfigVal{
			ID:         int(v.Id),
			Value:      v.Value,
			ResourceID: v.ResourceID,
			Prefix:     v.Prefix,
			Status:     v.Status,
		}
		if v.IsResource == 1 && v.ResourceID != 0 { // 替换资源模版名称
			val.IsResource = true
			res := ResourceSrv.FindResource(v.ResourceID)
			val.Value = codec.TomlVarEncode(res.Name)
		}
		res = append(res, db.ConfigData{
			Key:       v.Key,
			ConfigVal: val,
		})
	}
	return res, nil
}

// GetAppConfigText
func (cmc *confu) GetAppConfigText(caid int) (string, error) {
	var cmcApp db.CmcApp
	err := cmc.DB.Table("cmc_app").Select("*").Where("id=?", caid).Find(&cmcApp).Error
	if err != nil {
		return "", err
	}
	typ := cmcApp.Format

	dbHandler := cmc.DB.Table("cmc_config").Where("`caid` = ? AND status <> ?", caid, model.ItemStatusDel)
	var vals []db.CmcConfig
	err = dbHandler.Find(&vals).Error
	if err != nil {
		log.Warn("get cmc resources fail", "error", err)
		return "", err
	}
	var items []string
	for _, val := range vals {
		items = append(items, val.Value)
	}
	return parse.GetParseManage(string(typ)).Fusion(items)
}

// According to the application id and application name, get the configuration of all rings and equipment rooms
func (cmc *confu) GetAllConfigTextByApp(identify interface{}) (resp view.RespConfig, err error) {
	var (
		result     []db.CmcAppView
		configText string
	)
	appInfo, err := resource.Resource.GetApp(identify)
	if err != nil {
		return
	}
	// Get configuration information of all application rooms
	result, err = CmcAppSrv.AppConfigList("", "", appInfo.AppName, appInfo.Aid)
	resp = view.RespConfig{
		AppName: appInfo.AppName,
		Aid:     appInfo.Aid,
		Config:  make([]view.RespOneConfig, 0),
	}

	for _, value := range result {
		configText, err = cmc.GetAppConfigText(value.Id)
		if err != nil {
			return
		}
		resp.Config = append(resp.Config, view.RespOneConfig{
			Caid:     value.Id,
			Env:      value.Env,
			ZoneCode: value.ZoneCode,
			Content:  configText,
			AppName:  value.AppName,
			Aid:      value.Aid,
			Format:   string(value.Format),
			FileName: value.FileName,
		})

	}
	return
}

// 操作历史记录
func (c *confu) ChangeList(caid int, page, limit int) (result []db.CmcConfigLog, err error) {
	list := make([]db.CmcConfigLog, 0)
	offset := (page - 1) * limit
	sql := c.DB.Where("caid = ?", caid)
	if err = sql.Order("update_time desc").Offset(offset).Limit(limit).Find(&list).Error; err != nil {
		return
	}
	result = list
	return
}

func (c *confu) CmcAppDetail(id int) (result db.CmcAppView, err error) {
	result = db.CmcAppView{}
	c.DB.Table("cmc_app as a").
		Select("*").
		Joins("LEFT JOIN app as b ON a.aid = b.aid").
		Where("a.id = ?", id).
		Find(&result)
	return result, nil
}

func (c *confu) GetConfigItem(caid int, key string, id uint64) (res db.CmcConfig, err error) {
	err = c.DB.Table("cmc_config").
		Select("*").
		Where("`caid` = ? and `key` = ? and `id` <> ?", caid, key, id).
		Find(&res).Error
	if err != nil {
		return
	}
	return
}

func (c *confu) GetConfigTyp(id int) (typ string, err error) {
	result := db.CmcApp{}
	err = c.DB.Table("cmc_app as a").
		Select("*").
		Where("a.id = ?", id).
		Find(&result).Error
	if err != nil {
		return
	}
	return string(result.Format), nil
}

// UsingStatus Application configuration usage status
func (c *confu) UsingStatus(caid int) (result []db.DeployInstance, err error) {

	// Get file detail
	cmcApp, err := c.CmcAppDetail(caid)
	if err != nil {
		return result, err
	}
	if cmcApp.Id == 0 {
		return result, fmt.Errorf("no this app")
	}

	// Get a list of deployment instances
	deployList := deployInstanceList(cmcApp.AppName, cmcApp.Env, cmcApp.ZoneCode)

	//  Obtain the configuration synchronization list on ETCD
	etcdList, etcdListErr := etcdInstanceList(c.DB, cmcApp.AppName, cmcApp.FileName, cmcApp.Env, cmcApp.ZoneCode)
	if etcdListErr != nil {
		etcdList = make([]db.EtcdInstance, 0)
	}

	// Get the latest version of configure
	latestMd5, effectMD5, message, pubID := pubConfigLatestInfo(c.DB, caid)

	// Data aggregation
	for index, deployItem := range deployList {
		hostName := deployItem.HostName
		for _, etcdItem := range etcdList {
			if hostName == etcdItem.Host {
				deployList[index].MD5 = etcdItem.MD5
				deployList[index].Timestamp = etcdItem.Timestamp
				if effectMD5 == etcdItem.EffectMD5 {
					deployList[index].IsEffect = true
				} else {
					deployList[index].IsEffect = false
				}
				if latestMd5 == etcdItem.MD5 {
					deployList[index].IsLatest = true
					deployList[index].Message = message
					deployList[index].PubId = int(pubID)
					deployList[index].Params = etcdItem.Params
					deployList[index].ZoneCode = etcdItem.ZoneCode
				} else {
					_, oldMessage, oldPubID := pubConfigInfoByMD5(c.DB, caid, etcdItem.MD5)
					deployList[index].IsLatest = false
					deployList[index].Message = oldMessage
					deployList[index].PubId = int(oldPubID)
					deployList[index].Params = etcdItem.Params
					deployList[index].ZoneCode = etcdItem.ZoneCode
				}
			}
		}
		deployList[index].IsUse = usingInstanceList(c.DB, cmcApp.AppName, deployItem.HostName, cmcApp.FileName)
	}

	result = deployList

	// Aggregate start_time in app_node_info
	if len(result) > 0 {
		hostList := make([]string, 0)
		for _, v := range result {
			hostList = append(hostList, v.HostName)
		}
		hostInfo := make([]db.AppNodeInfo, 0)
		if err = c.DB.Table("app_node").Where("host_name in (?) and app_name = ?", hostList, cmcApp.AppName).Find(&hostInfo).Error; err != nil && err != gorm.ErrRecordNotFound {
			return
		}
		hostInfoMap := make(map[string]db.AppNodeInfo)
		for _, item := range hostInfo {
			hostInfoMap[item.HostName] = item
		}
		for index, item := range result {
			if nodeInfo, ok := hostInfoMap[item.HostName]; ok {
				result[index].ProcessStartTime = nodeInfo.UpdateTime
			}
		}
	}
	return
}

// StatusRefresh Refresh configuration access status
func (p *confu) StatusRefresh(caid int) (list []db.CmcUseStatus, err error) {
	list = make([]db.CmcUseStatus, 0)
	// Get file detail
	cmcApp, _ := p.CmcAppDetail(caid)
	if cmcApp.Id == 0 {
		err = fmt.Errorf("app not exists error")
		return
	}
	preNodes, _ := resource.Resource.GetAllAppNodeList(db.AppNode{
		AppName: cmcApp.AppName,
	})
	list, err = grpcgovern.IGrpcGovern.GetBatchPmtInfo(caid, &cmcApp, p.assemblyNodeParams(preNodes, cmcApp.Env))
	return
}

// 批量查询节点数据组装
func (c *confu) assemblyNodeParams(nodes []db.AppNode, env string) []db.AppNodeAgentView {
	agentPort := conf.GetInt("confgo.agent.port")
	fmt.Println("agentPort", agentPort)
	res := make([]db.AppNodeAgentView, 0)
	for _, node := range nodes {
		if node.Env == env {
			res = append(res, db.AppNodeAgentView{HostName: node.HostName, IpPort: node.Ip + fmt.Sprintf(":%d", agentPort)})
		}
	}
	return res
}

func (c *confu) UpdateNewStatus(list []db.CmcUseStatus) error {
	for _, item := range list {
		tempData := db.CmcUseStatus{}
		if err := c.DB.Where("app_name = ? AND hostname = ? and caid = ?", item.AppName, item.Hostname, item.Caid).First(&tempData).Error; err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if tempData.Id == 0 { // create
			if err := c.DB.Create(&item).Error; err != nil {
				return err
			}
		} else { // update
			tempData.ZoneCode = item.ZoneCode
			tempData.Content = item.Content
			tempData.IsUse = item.IsUse
			tempData.Extra = item.Extra
			if err := c.DB.Save(&tempData).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *confu) QueryConfigList(query map[string]interface{}) (result []db.CmcConfig) {
	result = make([]db.CmcConfig, 0)
	if err := c.DB.Where(query).Find(&result).Error; err != nil {
		return
	}
	return
}

func (c *confu) ResourceAppList(id uint64) (result []db.CmcAppView, err error) {
	configList := c.QueryConfigList(map[string]interface{}{
		"resource_id": id,
	})
	ids := make([]int, 0)
	for _, item := range configList {
		ids = append(ids, item.Caid)
	}
	result = make([]db.CmcAppView, 0)
	if err = CmcAppSrv.DB.Table("cmc_app as a").Select("*").Where("a.id IN (?)", ids).Joins("LEFT JOIN app as b ON a.aid = b.aid").Find(&result).Error; err != nil {
		return
	}
	return
}

// 发布成功后同步配置项状态
func (c *confu) SyncItemsStatus(caid int) error {
	tx := c.DB.Table("cmc_config").Begin()
	// 真删除配置项
	if err := tx.Where("caid = ? AND status = ?", caid, model.ItemStatusDel).Delete(db.CmcConfig{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	// 将其余配置项重置为发布
	if err := tx.Where("caid = ? ANd status <> ?", caid, model.ItemStatusDel).Updates(map[string]interface{}{"status": model.ItemStatusPub}).Error; err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

type itemDiffType int

const (
	itemDiffNew itemDiffType = iota
	itemDiffUpdate
	itemDiffDel
)

type configItemDiff struct {
	OpType itemDiffType
	db.CmcConfig
}

// AddWithTx ...
func (c *confu) AddWithTx(caid int, key, value string, resourceID int, opName string, tx *gorm.DB) (err error) {

	// Update config_cmc append config_cmc_log
	now := time.Now().Unix()

	item := db.CmcConfig{
		Caid:       caid,
		Key:        key,
		Value:      value,
		Status:     model.ItemStatusNew,
		UpdateTime: now,
		OpName:     opName,
	}

	// 注入资源字段
	if resourceID != 0 {
		item.IsResource = 1
		item.ResourceID = resourceID
	}
	if err = tx.Create(&item).Error; err != nil {
		return
	}
	itemlog := db.CmcConfigLog{
		Caid:       caid,
		Key:        key,
		NewValue:   value,
		OpType:     model.ItemLogStatusNew,
		UpdateTime: now,
		OpName:     opName,
	}
	if err = tx.Create(&itemlog).Error; err != nil {
		return
	}
	return nil
}

func (c *confu) UpdateWithTx(id uint64, caid int, key, value string, resourceID int, opName string, tx *gorm.DB) (err error) {
	oldItem := db.CmcConfig{}
	tx.Where("id = ?", id).First(&oldItem)
	if oldItem.Id == 0 {
		return fmt.Errorf("id is 0")
	}
	if oldItem.Value == value && oldItem.ResourceID == resourceID { // 没有变化
		return nil
	}

	// 更新配置状态
	now := time.Now().Unix()

	// 获取旧值
	oldValue := oldItem.Value

	// 旧注释

	oldItem.Status = model.ItemStatusUpdate
	oldItem.Value = value
	oldItem.OpName = opName
	// 注入资源字段
	if resourceID != 0 {
		oldItem.ResourceID = resourceID
		oldItem.IsResource = 1
	} else {
		oldItem.ResourceID = 0
		oldItem.IsResource = 0
	}

	if err = tx.Save(oldItem).Error; err != nil {
		return
	}
	itemLog := db.CmcConfigLog{
		Caid:       oldItem.Caid,
		Key:        oldItem.Key,
		OldValue:   oldValue,
		NewValue:   value,
		OpType:     model.ItemLogStatusUpdate,
		UpdateTime: now,
		OpName:     opName,
	}
	if err = tx.Create(&itemLog).Error; err != nil {
		return
	}
	return
}

// 删除配置
func (c *confu) DeleteWithTx(id uint64, opName string, tx *gorm.DB) (err error) {
	// todo error
	if id == 0 {
		return fmt.Errorf("id is 0")
	}

	oldItem := db.CmcConfig{}
	c.DB.Where("id = ?", id).First(&oldItem)
	if oldItem.Id == 0 {
		return fmt.Errorf("item is not exists")
	}

	now := time.Now().Unix()

	// 新增状态下的配置可以直接删除
	if oldItem.Status == model.ItemStatusNew {
		if err = tx.Where("id = ?", id).Delete(&oldItem).Error; err != nil {
			tx.Rollback()
			return
		}
	} else {
		oldItem.Status = model.ItemStatusDel
		if err = tx.Save(&oldItem).Error; err != nil {
			tx.Rollback()
			return
		}
	}

	itemlog := db.CmcConfigLog{
		Caid:       oldItem.Caid,
		Key:        oldItem.Key,
		OldValue:   oldItem.Value,
		OpType:     model.ItemLogStatusDel,
		UpdateTime: now,
		OpName:     opName,
	}
	if err = tx.Create(&itemlog).Error; err != nil {
		tx.Rollback()
		return
	}
	tx.Commit()
	return
}

// 删除配置
func (c *confu) Del(id uint64, opName string) (err error) {
	// todo error
	if id == 0 {
		return fmt.Errorf("id is 0")
	}

	oldItem := db.CmcConfig{}
	c.DB.Where("id = ?", id).First(&oldItem)
	if oldItem.Id == 0 {
		return fmt.Errorf("item is not exists")
	}

	tx := c.DB.Begin()
	now := time.Now().Unix()

	// 新增状态下的配置可以直接删除
	if oldItem.Status == model.ItemStatusNew {
		if err = tx.Where("id = ?", id).Delete(&oldItem).Error; err != nil {
			tx.Rollback()
			return
		}
	} else {
		oldItem.Status = model.ItemStatusDel
		if err = tx.Save(&oldItem).Error; err != nil {
			tx.Rollback()
			return
		}
	}

	itemlog := db.CmcConfigLog{
		Caid:       oldItem.Caid,
		Key:        oldItem.Key,
		OldValue:   oldItem.Value,
		OpType:     model.ItemLogStatusDel,
		UpdateTime: now,
		OpName:     opName,
	}
	if err = tx.Create(&itemlog).Error; err != nil {
		tx.Rollback()
		return
	}
	tx.Commit()
	return
}

// 辅助函数
func JSONMarshal(v interface{}, safeEncoding bool) string {
	b, _ := json.Marshal(v)
	if safeEncoding {
		b = bytes.Replace(b, []byte("\\u003c"), []byte("<"), -1)
		b = bytes.Replace(b, []byte("\\u003e"), []byte(">"), -1)
		b = bytes.Replace(b, []byte("\\u0026"), []byte("&"), -1)
	}
	return string(b)
}

func findResourceByName(value string, valueType string) (resourceValue string, resourceType string, resourceID int, isResource int, err error) {
	if codec.IsTemplateStr(value) { //
		name := codec.VarDecode(value)
		res := ResourceSrv.QueryResource(map[string]interface{}{
			"name": name,
		})
		if res.Id == 0 {
			err = fmt.Errorf("not found resources")
			return
		}
		return value, res.ValueType, int(res.Id), 1, nil
	}
	return value, valueType, 0, 0, nil
}

func pubConfigLatestInfo(gormdb *gorm.DB, caid int) (md5, effectMD5 string, message string, pubID int) {
	// 匹配md5
	dbConn := gormdb.Table("cmc_history")
	messageData := db.CmcHistory{}
	dbConn.Where("caid = ?", caid).Order("create_time desc").First(&messageData)
	return messageData.Md5, messageData.EffectMd5, messageData.Message, messageData.Id
}

func pubConfigInfoByMD5(gormdb *gorm.DB, caid int, itemMd5 string) (md5 string, message string, pubID int) {
	// 匹配md5
	dbConn := gormdb.Table("cmc_history")
	messageData := db.CmcHistory{}
	dbConn.Where("caid = ? AND md5 = ?", caid, itemMd5).Order("create_time desc").First(&messageData)
	return messageData.Md5, messageData.Message, messageData.Id
}

func etcdInstanceList(gormdb *gorm.DB, appName, fileName, env, zoneCode string) (result []db.EtcdInstance, err error) {

	data, err := grpcgovern.IGrpcGovern.GetConfigStatus(env, zoneCode, appName, fileName)

	if err != nil {
		return
	}
	// 正常结果

	// supervisor状态列表
	statusList := make([]db.CmcUseStatus, 0)
	gormdb.Table("cmc_use_status").Where("app_name = ? AND env = ? AND zone_code = ?", appName, env, zoneCode).Find(&statusList)
	statusMap := make(map[string]db.CmcUseStatus, 0)
	for _, item := range statusList {
		statusMap[item.Hostname] = item
	}

	findHostItem := func(hostname string) db.CmcUseStatus {
		if item, ok := statusMap[hostname]; ok {
			return item
		}
		return db.CmcUseStatus{}
	}

	result = make([]db.EtcdInstance, 0)
	for _, item := range data {
		statusItem := findHostItem(item.Hostname)
		result = append(result, db.EtcdInstance{
			Host:      item.Hostname,
			MD5:       item.Md5,
			Timestamp: item.Timestamp,
			Params:    statusItem.Content,
			ZoneCode:  statusItem.ZoneCode,
			EffectMD5: item.EffectMD5,
		})
	}
	return
}

func deployInstanceList(appName, env, zoneCode string) (result []db.DeployInstance) {
	preNodes, _ := resource.Resource.GetAllAppNodeList(db.AppNode{
		AppName:  appName,
		Env:      env,
		ZoneCode: zoneCode,
	})
	for _, node := range preNodes {
		result = append(result, db.DeployInstance{
			HostName: node.HostName,
		})
	}
	return
}

func usingInstanceList(gormdb *gorm.DB, appName string, hostname string, fileName string) (isUse bool) {
	configStatus := db.CmcUseStatus{}
	if err := gormdb.Where("app_name = ? AND hostname = ?", appName, hostname).First(&configStatus).Error; err != nil {
		return false
	}
	if strings.Contains(configStatus.Content, fileName) && strings.Contains(configStatus.Content, conf.GetString("confgo.dir")) { // //接入配置中心
		return true
	}
	return false
}

// 替换关联资源发布的时候替换，存一份对外的模版diff文档
func FindResource(list []db.CmcResource, call func(val db.CmcResource) bool) (resource db.CmcResource) {
	resource = db.CmcResource{}
	for _, item := range list {
		if call(item) {
			resource = item
		}
	}
	return
}

func GetConfig(configId int, itemId int) (text string, err error) {
	typ, err := ConfuSrv.GetConfigTyp(configId)
	if err != nil {
		return
	}
	// 获取配置k-v列表
	appConfigKv, err := ConfuSrv.GetAppKVs(configId, itemId)
	if err != nil {
		return
	}
	text, _, err = FormatByKvs(appConfigKv, typ, "")
	if err != nil {
		return
	}
	return text, nil
}

func GenConfig(configId int, itemId int, value string) (text string, err error) {
	typ, err := ConfuSrv.GetConfigTyp(configId)
	if err != nil {
		return
	}
	// 获取配置k-v列表
	appConfigKv, err := ConfuSrv.GetAppKVs(configId, itemId)
	if err != nil {
		return
	}
	text, _, err = FormatByKvs(appConfigKv, typ, value)
	if err != nil {
		return
	}
	return text, nil
}

func FormatByKvs(items []db.CmcConfig, format string, value string) (text, md5Str string, err error) {
	// 获取资源列表
	var sources []string
	for _, v := range items {
		sources = append(sources, v.Value)
	}
	if value != "" {
		sources = append(sources, value)
	}
	text, err = parse.GetParseManage(format).Fusion(sources)
	if err != nil {
		return
	}
	// 计算md5值
	h := md5.New()
	if _, err = io.WriteString(h, text); err != nil {
		return
	}
	return text, fmt.Sprintf("%x", h.Sum(nil)), nil
}

func PublishSign(text string, textMd5, format string) (signText, md5Str string, err error) {
	var sign struct {
		JunoAgentDate int64  `json:"juno_agent_date"`
		JunoAgentMD5  string `json:"juno_agent_md5"`
	}
	sign.JunoAgentDate = time.Now().Unix()
	sign.JunoAgentMD5 = textMd5

	signJson, _ := json.Marshal(sign)

	tmp := make([]string, 0)
	tmp = append(tmp, string(signJson))

	signText, err = parse.GetParseManage(format).FusionWithTpl(text, tmp)
	if err != nil {
		return
	}
	// 计算md5值
	h := md5.New()
	if _, err = io.WriteString(h, signText); err != nil {
		return
	}
	return text, fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (cmc *confu) GetCmcStat(start, end int64) ([]db.AppCmcStat, []db.CmcCnt, int, error) {
	dbConn := cmc.DB.Table("cmc_app")
	// var res = make(map[string]model.ConfigVal)
	var (
		res    = make([]db.AppCmcStat, 0)
		cmcCnt = make([]db.CmcCnt, 0)
		total  = 0
	)

	if err := dbConn.Count(&total).Error; err != nil {
		return res, cmcCnt, total, err
	}

	if err := dbConn.Select("env,count(id) as cnt").Group("env").Find(&res).Error; err != nil {
		return res, cmcCnt, total, err
	}

	if err := dbConn.Select("DATE_FORMAT(FROM_UNIXTIME(create_time), '%Y-%m-%d') as day_time, count(id) as cnt").
		Where("create_time between ? and ?", start, end).Group("day_time").Find(&cmcCnt).Error; err != nil {
		return res, cmcCnt, total, err
	}
	return res, cmcCnt, total, nil

}
