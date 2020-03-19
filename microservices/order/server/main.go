package main

import (
	"context"
	"log"
	"net"
	"os"
	"strconv"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/golang/protobuf/ptypes/timestamp"
	_ "github.com/jinzhu/gorm/dialects/mysql"
	"github.com/kubernetes-native-testbed/kubernetes-native-testbed/microservices/order"
	pb "github.com/kubernetes-native-testbed/kubernetes-native-testbed/microservices/order/protobuf"
	"google.golang.org/grpc"
	health "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	dbUsername string
	dbPassword string
	dbName     string
	dbHost     string
	dbPort     int

	deliveryStatusUsername string
	deliveryStatusPassword string
	deliveryStatusHost     string
	deliveryStatusPort     int
	deliveryStatusSubject  string
)

const (
	defaultBindAddr = ":8080"

	componentName     = "order"
	defaultDBUsername = componentName + "user"
	defaultDBPassword = componentName
	defaultDBName     = componentName + "db"
	defaultDBHost     = componentName
	defaultDBPort     = 4000

	defaultDeliveryStatusUsername = componentName
	defaultDeliveryStatusPassword = componentName
	defaultDeliveryStatusHost     = "delivery-status-queue.delivery-status.svc.cluster.local"
	defaultDeliveryStatusPort     = 4222
	defaultDeliveryStatusSubject  = componentName
)

func init() {
	var err error
	if dbUsername = os.Getenv("DB_USERNAME"); dbUsername == "" {
		dbUsername = defaultDBUsername
	}
	if dbPassword = os.Getenv("DB_PASSWORD"); dbPassword == "" {
		dbPassword = defaultDBPassword
	}
	if dbName = os.Getenv("DB_NAME"); dbName == "" {
		dbName = defaultDBName
	}
	if dbHost = os.Getenv("DB_HOST"); dbHost == "" {
		dbHost = defaultDBHost
	}
	if dbPort, err = strconv.Atoi(os.Getenv("DB_PORT")); err != nil {
		dbPort = defaultDBPort
		log.Printf("dbPort parse error: %v", err)
	}
	if deliveryStatusUsername = os.Getenv("DELIVERY_STATUS_USERNAME"); deliveryStatusUsername == "" {
		deliveryStatusUsername = defaultDeliveryStatusUsername
	}
	if deliveryStatusPassword = os.Getenv("DELIVERY_STATUS_PASSWORD"); deliveryStatusPassword == "" {
		deliveryStatusPassword = defaultDeliveryStatusPassword
	}
	if deliveryStatusHost = os.Getenv("DELIVERY_STATUS_HOST"); deliveryStatusHost == "" {
		deliveryStatusHost = defaultDeliveryStatusHost
	}
	if deliveryStatusPort, err = strconv.Atoi(os.Getenv("DELIVERY_STATUS_PORT")); err != nil {
		deliveryStatusPort = defaultDeliveryStatusPort
		log.Printf("deliveryStatusPort parse error: %v", err)
	}
	if deliveryStatusSubject = os.Getenv("DELIVERY_STATUS_SUBJECT"); deliveryStatusSubject == "" {
		deliveryStatusSubject = defaultDeliveryStatusSubject
	}
}

type orderAPIServer struct {
	orderRepository              order.OrderRepository
	orderSenderForDeliveryStatus order.OrderSender
}

func (s *orderAPIServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	uuid := req.GetUUID()
	o, err := s.orderRepository.FindByUUID(uuid)
	if err != nil {
		return &pb.GetResponse{}, err
	}
	log.Printf("get %s", o)

	var resp pb.GetResponse
	var cat, uat, dat *timestamp.Timestamp
	if cat, err = ptypes.TimestampProto(o.CreatedAt); err != nil {
		return &pb.GetResponse{}, err
	}
	if uat, err = ptypes.TimestampProto(o.UpdatedAt); err != nil {
		return &pb.GetResponse{}, err
	}
	if o.DeletedAt != nil {
		if dat, err = ptypes.TimestampProto(*o.DeletedAt); err != nil {
			return &pb.GetResponse{}, err
		}
	}

	orderedProducts := make([]*pb.OrderedProduct, len(o.OrderedProducts))
	for i := range o.OrderedProducts {
		op := o.OrderedProducts[i]

		var opcat, opuat, opdat *timestamp.Timestamp
		if opcat, err = ptypes.TimestampProto(op.CreatedAt); err != nil {
			return &pb.GetResponse{}, err
		}
		if opuat, err = ptypes.TimestampProto(op.UpdatedAt); err != nil {
			return &pb.GetResponse{}, err
		}
		if op.DeletedAt != nil {
			if opdat, err = ptypes.TimestampProto(*op.DeletedAt); err != nil {
				return &pb.GetResponse{}, err
			}
		}

		orderedProducts[i] = &pb.OrderedProduct{
			OrderUUID:   op.OrderUUID,
			ProductUUID: op.ProductUUID,
			Count:       int32(op.Count),
			Price:       int32(op.Price),
			CreatedAt:   opcat,
			UpdatedAt:   opuat,
			DeletedAt:   opdat,
		}
	}

	resp.Order = &pb.Order{
		UUID:            o.UUID,
		OrderedProducts: orderedProducts,
		UserUUID:        o.UserUUID,
		PaymentInfoUUID: o.PaymentInfoUUID,
		AddressUUID:     o.AddressUUID,
		CreatedAt:       cat,
		UpdatedAt:       uat,
		DeletedAt:       dat,
	}

	return &resp, nil
}

func (s *orderAPIServer) Set(ctx context.Context, req *pb.SetRequest) (*pb.SetResponse, error) {
	orderedProducts := make([]order.OrderedProduct, len(req.GetOrder().GetOrderedProducts()))
	for i, op := range req.GetOrder().GetOrderedProducts() {
		orderedProducts[i] = order.OrderedProduct{
			OrderUUID:   op.GetOrderUUID(),
			ProductUUID: op.GetProductUUID(),
			Count:       int(op.GetCount()),
			Price:       int(op.GetPrice()),
		}
	}

	o := &order.Order{
		UUID:            req.GetOrder().GetUUID(),
		OrderedProducts: orderedProducts,
		UserUUID:        req.GetOrder().GetUserUUID(),
		PaymentInfoUUID: req.GetOrder().GetPaymentInfoUUID(),
		AddressUUID:     req.GetOrder().GetAddressUUID(),
	}
	log.Printf("set %s", o)

	uuid, err := s.orderRepository.Store(o)
	if err != nil {
		return &pb.SetResponse{}, err
	}

	go func() {
		if err := s.orderSenderForDeliveryStatus.Send(o); err != nil {
			// TODO: save fail information to order table for avoding lost order
			log.Printf("send error: %v", err)
		}
	}()

	return &pb.SetResponse{UUID: uuid}, nil
}

func (s *orderAPIServer) Update(ctx context.Context, req *pb.UpdateRequest) (*empty.Empty, error) {
	orderedProducts := make([]order.OrderedProduct, len(req.GetOrder().GetOrderedProducts()))
	for i, op := range req.GetOrder().GetOrderedProducts() {
		orderedProducts[i] = order.OrderedProduct{
			OrderUUID:   op.GetOrderUUID(),
			ProductUUID: op.GetProductUUID(),
			Count:       int(op.GetCount()),
			Price:       int(op.GetPrice()),
		}
	}

	o := &order.Order{
		UUID:            req.GetOrder().GetUUID(),
		OrderedProducts: orderedProducts,
		UserUUID:        req.GetOrder().GetUserUUID(),
		PaymentInfoUUID: req.GetOrder().GetPaymentInfoUUID(),
		AddressUUID:     req.GetOrder().GetAddressUUID(),
	}
	log.Printf("update %s", o)

	if err := s.orderRepository.Update(o); err != nil {
		return &empty.Empty{}, err
	}

	return &empty.Empty{}, nil
}

func (s *orderAPIServer) Delete(ctx context.Context, req *pb.DeleteRequest) (*empty.Empty, error) {
	uuid := req.GetUUID()
	log.Printf("delete {\"uuid\":\"%s\"}", uuid)

	if err := s.orderRepository.DeleteByUUID(uuid); err != nil {
		return &empty.Empty{}, err
	}

	return &empty.Empty{}, nil
}

func main() {
	lis, err := net.Listen("tcp", defaultBindAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	log.Printf("listen on %s", defaultBindAddr)

	tidbConfig := order.OrderRepositoryTiDBConfig{
		Host:     dbHost,
		Port:     dbPort,
		Username: dbUsername,
		Password: dbPassword,
		DBName:   dbName,
	}
	tidb, closeTiDB, err := tidbConfig.Connect()
	if err != nil {
		log.Fatalf("failed to open database: %v (config=%#v)", err, tidbConfig)
	}
	defer closeTiDB()
	log.Printf("succeed to open database")

	natsConfig := order.OrderSenderNATSConfig{
		Host:     deliveryStatusHost,
		Port:     deliveryStatusPort,
		Username: deliveryStatusUsername,
		Password: deliveryStatusPassword,
		Subject:  deliveryStatusSubject,
		Retry:    5,
	}
	nats, closeNats, err := natsConfig.Connect()
	if err != nil {
		log.Fatalf("failed to create connection to delivery status queue: %v (config=%#v)", err, natsConfig)
	}
	defer closeNats()
	log.Printf("succeed to connect to delivery status queue")

	s := grpc.NewServer()
	api := &orderAPIServer{
		orderRepository:              tidb,
		orderSenderForDeliveryStatus: nats,
	}
	pb.RegisterOrderAPIServer(s, api)

	healthpb.RegisterHealthServer(s, health.NewServer())

	log.Printf("setup database")
	if err := api.orderRepository.InitDB(); err != nil {
		log.Fatalf("failed to init database: %v", err)
	}

	log.Printf("start order API server")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
