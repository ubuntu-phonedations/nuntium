/*
 * Copyright 2014 Canonical Ltd.
 *
 * Authors:
 * Sergio Schvezov: sergio.schvezov@cannical.com
 *
 * This file is part of nuntium.
 *
 * nuntium is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; version 3.
 *
 * nuntium is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"os/user"

	"github.com/ubuntu-phonedations/nuntium/mms"
	"github.com/ubuntu-phonedations/nuntium/ofono"
	"github.com/ubuntu-phonedations/nuntium/storage"
	"github.com/ubuntu-phonedations/nuntium/telepathy"

	"launchpad.net/go-dbus/v1"
)

type Mediator struct {
	modem               *ofono.Modem
	telepathyService    *telepathy.MMSService
	NewMNotificationInd chan *mms.MNotificationInd
	NewMSendReq         chan *mms.MSendReq
	NewMSendReqFile     chan struct{ filePath, uuid string }
	outMessage          chan *telepathy.OutgoingMessage
	terminate           chan bool
	contextLock         sync.Mutex
}

//TODO these vars need a configuration location managed by system settings or
//some UI accessible location.
//useDeliveryReports is set in ofono
var (
	deferredDownload   bool
	useDeliveryReports bool
)

func NewMediator(modem *ofono.Modem) *Mediator {
	mediator := &Mediator{modem: modem}
	mediator.NewMNotificationInd = make(chan *mms.MNotificationInd)
	mediator.NewMSendReq = make(chan *mms.MSendReq)
	mediator.NewMSendReqFile = make(chan struct{ filePath, uuid string })
	mediator.outMessage = make(chan *telepathy.OutgoingMessage)
	mediator.terminate = make(chan bool)
	return mediator
}

func (mediator *Mediator) Delete() {
	mediator.terminate <- mediator.telepathyService == nil
}

func (mediator *Mediator) init(mmsManager *telepathy.MMSManager) {
mediatorLoop:
	for {
		select {
		case push, ok := <-mediator.modem.PushAgent.Push:
			if !ok {
				log.Print("PushChannel is closed")
				continue
			}
			if !mmsEnabled() {
				continue
			}
			go mediator.handleMNotificationInd(push)
		case mNotificationInd := <-mediator.NewMNotificationInd:
			if deferredDownload {
				go mediator.handleDeferredDownload(mNotificationInd)
			} else {
				go mediator.getMRetrieveConf(mNotificationInd)
			}
		case msg := <-mediator.outMessage:
			go mediator.handleOutgoingMessage(msg)
		case mSendReq := <-mediator.NewMSendReq:
			go mediator.handleMSendReq(mSendReq)
		case mSendReqFile := <-mediator.NewMSendReqFile:
			go mediator.sendMSendReq(mSendReqFile.filePath, mSendReqFile.uuid)
		case id := <-mediator.modem.IdentityAdded:
			var err error
			mediator.telepathyService, err = mmsManager.AddService(id, mediator.modem.Modem, mediator.outMessage, useDeliveryReports)
			if err != nil {
				log.Fatal(err)
			}
		case id := <-mediator.modem.IdentityRemoved:
			err := mmsManager.RemoveService(id)
			if err != nil {
				log.Fatal(err)
			}
			mediator.telepathyService = nil
		case ok := <-mediator.modem.PushInterfaceAvailable:
			if ok {
				if err := mediator.modem.PushAgent.Register(); err != nil {
					log.Fatal(err)
				}
			} else {
				if err := mediator.modem.PushAgent.Unregister(); err != nil {
					log.Fatal(err)
				}
			}
		case terminate := <-mediator.terminate:
			/*
				close(mediator.terminate)
				close(mediator.outMessage)
				close(mediator.NewMNotificationInd)
				close(mediator.NewMRetrieveConf)
				close(mediator.NewMRetrieveConfFile)
				close(mediator.NewMSendReq)
				close(mediator.NewMSendReqFile)
			*/
			if terminate {
				break mediatorLoop
			}
		}
	}
	log.Print("Ending mediator instance loop for modem")
}

func (mediator *Mediator) handleMNotificationInd(pushMsg *ofono.PushPDU) {
	if pushMsg == nil {
		log.Print("Received nil push")
		return
	}
	dec := mms.NewDecoder(pushMsg.Data)
	mNotificationInd := mms.NewMNotificationInd()
	if err := dec.Decode(mNotificationInd); err != nil {
		log.Println("Unable to decode m-notification.ind: ", err, "with log", dec.GetLog())
		return
	}
	storage.Create(mNotificationInd.UUID, mNotificationInd.ContentLocation)
	mediator.NewMNotificationInd <- mNotificationInd
}

func (mediator *Mediator) handleDeferredDownload(mNotificationInd *mms.MNotificationInd) {
	//TODO send MessageAdded with status="deferred" and mNotificationInd relevant headers
}

func (mediator *Mediator) getMRetrieveConf(mNotificationInd *mms.MNotificationInd) {
	mediator.contextLock.Lock()
	defer mediator.contextLock.Unlock()

	var proxy ofono.ProxyInfo
	var mmsContext ofono.OfonoContext

	if mNotificationInd.IsLocal() {
		log.Print("This is a local test, skipping context activation and proxy settings")
	} else {
		var err error
		preferredContext, _ := mediator.telepathyService.GetPreferredContext()
		mmsContext, err = mediator.modem.ActivateMMSContext(preferredContext)
		if err != nil {
			log.Print("Cannot activate ofono context: ", err)
			return
		}
		defer func() {
			if err := mediator.modem.DeactivateMMSContext(mmsContext); err != nil {
				log.Println("Issues while deactivating context:", err)
			}
		}()

		if err := mediator.telepathyService.SetPreferredContext(mmsContext.ObjectPath); err != nil {
			log.Println("Unable to store the preferred context for MMS:", err)
		}
		proxy, err = mmsContext.GetProxy()
		if err != nil {
			log.Print("Error retrieving proxy: ", err)
			return
		}
	}

	if filePath, err := mNotificationInd.DownloadContent(proxy.Host, int32(proxy.Port)); err != nil {
		//TODO telepathy service signal the download error
		log.Print("Download issues: ", err)
		return
	} else {
		if err := storage.UpdateDownloaded(mNotificationInd.UUID, filePath); err != nil {
			log.Println("When calling UpdateDownloaded: ", err)
			return
		}
	}

	mRetrieveConf, err := mediator.handleMRetrieveConf(mNotificationInd.UUID)
	if err != nil {
		log.Print(err)
		return
	}

	mNotifyRespInd := mRetrieveConf.NewMNotifyRespInd(useDeliveryReports)
	if err := storage.UpdateRetrieved(mNotifyRespInd.UUID); err != nil {
		log.Print("Can't update mms status: ", err)
		return
	}

	if !mNotificationInd.IsLocal() {
		// TODO deferred case
		filePath := mediator.handleMNotifyRespInd(mNotifyRespInd)
		if filePath == "" {
			return
		}
		mediator.sendMNotifyRespInd(filePath, &mmsContext)
	} else {
		log.Print("This is a local test, skipping m-notifyresp.ind")
	}
}

func (mediator *Mediator) handleMRetrieveConf(uuid string) (*mms.MRetrieveConf, error) {
	var filePath string
	if f, err := storage.GetMMS(uuid); err == nil {
		filePath = f
	} else {
		return nil, fmt.Errorf("unable to retrieve MMS: %s", err)
	}

	mmsData, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("issues while reading from downloaded file: %s", err)
	}

	mRetrieveConf := mms.NewMRetrieveConf(uuid)
	dec := mms.NewDecoder(mmsData)
	if err := dec.Decode(mRetrieveConf); err != nil {
		return nil, fmt.Errorf("unable to decode m-retrieve.conf: %s with log %s", err, dec.GetLog())
	}

	if mediator.telepathyService != nil {
		if err := mediator.telepathyService.IncomingMessageAdded(mRetrieveConf); err != nil {
			log.Println("Cannot notify telepathy-ofono about new message", err)
		}
	} else {
		log.Print("Not sending recently retrieved message")
	}

	return mRetrieveConf, nil
}

func (mediator *Mediator) handleMNotifyRespInd(mNotifyRespInd *mms.MNotifyRespInd) string {
	f, err := storage.CreateResponseFile(mNotifyRespInd.UUID)
	if err != nil {
		log.Print("Unable to create m-notifyresp.ind file for ", mNotifyRespInd.UUID)
		return ""
	}
	enc := mms.NewEncoder(f)
	if err := enc.Encode(mNotifyRespInd); err != nil {
		log.Print("Unable to encode m-notifyresp.ind for ", mNotifyRespInd.UUID)
		f.Close()
		return ""
	}
	filePath := f.Name()
	if err := f.Sync(); err != nil {
		log.Print("Error while syncing", f.Name(), ": ", err)
		return ""
	}
	if err := f.Close(); err != nil {
		log.Print("Error while closing", f.Name(), ": ", err)
		return ""
	}
	log.Printf("Created %s to handle m-notifyresp.ind for %s", filePath, mNotifyRespInd.UUID)
	return filePath
}

func (mediator *Mediator) sendMNotifyRespInd(filePath string, mmsContext *ofono.OfonoContext) {
	defer os.Remove(filePath)

	proxy, err := mmsContext.GetProxy()
	if err != nil {
		log.Println("Cannot retrieve MMS proxy setting", err)
		return
	}
	msc, err := mmsContext.GetMessageCenter()
	if err != nil {
		log.Println("Cannot retrieve MMSC setting", err)
		return
	}

	if _, err := mms.Upload(filePath, msc, proxy.Host, int32(proxy.Port)); err != nil {
		log.Printf("Cannot upload m-notifyresp.ind encoded file %s to message center: %s", filePath, err)
	}
}

func (mediator *Mediator) handleOutgoingMessage(msg *telepathy.OutgoingMessage) {
	var cts []*mms.Attachment
	for _, att := range msg.Attachments {
		ct, err := mms.NewAttachment(att.Id, att.ContentType, att.FilePath)
		if err != nil {
			log.Print(err)
			//TODO reply to telepathy ofono with an error
			return
		}
		cts = append(cts, ct)
	}
	mSendReq := mms.NewMSendReq(msg.Recipients, cts, useDeliveryReports)
	if _, err := mediator.telepathyService.ReplySendMessage(msg.Reply, mSendReq.UUID); err != nil {
		log.Print(err)
		return
	}
	mediator.NewMSendReq <- mSendReq
}

func (mediator *Mediator) handleMSendReq(mSendReq *mms.MSendReq) {
	log.Print("Encoding M-Send.Req")
	f, err := storage.CreateSendFile(mSendReq.UUID)
	if err != nil {
		log.Print("Unable to create m-send.req file for ", mSendReq.UUID)
		return
	}
	defer f.Close()
	enc := mms.NewEncoder(f)
	if err := enc.Encode(mSendReq); err != nil {
		log.Print("Unable to encode m-send.req for ", mSendReq.UUID)
		if err := mediator.telepathyService.MessageStatusChanged(mSendReq.UUID, telepathy.PERMANENT_ERROR); err != nil {
			log.Println(err)
		}
		f.Close()
		return
	}
	filePath := f.Name()
	if err := f.Sync(); err != nil {
		log.Print("Error while syncing", f.Name(), ": ", err)
		return
	}
	if err := f.Close(); err != nil {
		log.Print("Error while closing", f.Name(), ": ", err)
		return
	}
	log.Printf("Created %s to handle m-send.req for %s", filePath, mSendReq.UUID)
	mediator.sendMSendReq(filePath, mSendReq.UUID)
}

func (mediator *Mediator) sendMSendReq(mSendReqFile, uuid string) {
	defer os.Remove(mSendReqFile)
	defer mediator.telepathyService.MessageDestroy(uuid)
	mSendConfFile, err := mediator.uploadFile(mSendReqFile)
	if err != nil {
		if err := mediator.telepathyService.MessageStatusChanged(uuid, telepathy.TRANSIENT_ERROR); err != nil {
			log.Println(err)
		}
		log.Printf("Cannot upload m-send.req encoded file %s to message center: %s", mSendReqFile, err)
		return
	}

	defer os.Remove(mSendConfFile)
	mSendConf, err := parseMSendConfFile(mSendConfFile)
	if err != nil {
		log.Println("Error while decoding m-send.conf:", err)
		if err := mediator.telepathyService.MessageStatusChanged(uuid, telepathy.TRANSIENT_ERROR); err != nil {
			log.Println(err)
		}
		return
	}

	log.Println("m-send.conf ResponseStatus for", uuid, "is", mSendConf.ResponseStatus)
	var status string
	switch mSendConf.Status() {
	case nil:
		status = telepathy.SENT
	case mms.ErrPermanent:
		status = telepathy.PERMANENT_ERROR
	case mms.ErrTransient:
		status = telepathy.TRANSIENT_ERROR
	}
	if err := mediator.telepathyService.MessageStatusChanged(uuid, status); err != nil {
		log.Println(err)
	}
}

func parseMSendConfFile(mSendConfFile string) (*mms.MSendConf, error) {
	b, err := ioutil.ReadFile(mSendConfFile)
	if err != nil {
		return nil, err
	}

	mSendConf := mms.NewMSendConf()

	dec := mms.NewDecoder(b)
	if err := dec.Decode(mSendConf); err != nil {
		return nil, err
	}
	return mSendConf, nil
}

func (mediator *Mediator) uploadFile(filePath string) (string, error) {
	mediator.contextLock.Lock()
	defer mediator.contextLock.Unlock()

	preferredContext, _ := mediator.telepathyService.GetPreferredContext()
	mmsContext, err := mediator.modem.ActivateMMSContext(preferredContext)
	if err != nil {
		return "", err
	}
	if err := mediator.telepathyService.SetPreferredContext(mmsContext.ObjectPath); err != nil {
		log.Println("Unable to store the preferred context for MMS:", err)
	}
	defer func() {
		if err := mediator.modem.DeactivateMMSContext(mmsContext); err != nil {
			log.Println("Issues while deactivating context:", err)
		}
	}()

	proxy, err := mmsContext.GetProxy()
	if err != nil {
		return "", err
	}
	msc, err := mmsContext.GetMessageCenter()
	if err != nil {
		return "", err
	}
	mSendRespFile, uploadErr := mms.Upload(filePath, msc, proxy.Host, int32(proxy.Port))

	return mSendRespFile, uploadErr
}

// by default this method returns true, unless it is strictly requested to disable
func mmsEnabled() (bool) {
	conn, err := dbus.Connect(dbus.SystemBus)
	if err != nil {
	        return true
	}
	usr, err := user.Current()
	if err != nil {
		return true
	}
	activeUser := dbus.ObjectPath("/org/freedesktop/Accounts/User" + usr.Uid)

	obj := conn.Object("org.freedesktop.Accounts", activeUser)

	reply, err := obj.Call("org.freedesktop.DBus.Properties", "Get", "com.ubuntu.touch.AccountsService.Phone", "MmsEnabled")
	if err != nil || reply.Type == dbus.TypeError {
		fmt.Println(fmt.Errorf("failed to get mms option: %s", err))
		return true
	}
	mms := dbus.Variant{true}
	if err := reply.Args(&mms); err == nil {
		return mms.Value.(bool)
	}

	return true
}
