// Package main is an example of a custom viam server.

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/edaniels/golog"
	"github.com/go-daq/canbus"
	"github.com/golang/geo/r3"
	goutils "go.viam.com/utils"

	"go.viam.com/rdk/components/base"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/config"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/registry"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/utils"
)

var model = resource.NewModel("viamlabs", "tutorial", "intermode")

// boilerplate to make this exist as a component.
func init() {
	registry.RegisterComponent(
		base.Subtype,
		model,
		registry.Component{Constructor: func(
			ctx context.Context,
			deps registry.Dependencies,
			config config.Component,
			logger golog.Logger,
		) (interface{}, error) {
			return newBase(config.Name, logger)
		}})
}

func main() {
	goutils.ContextualMain(mainWithArgs, golog.NewDevelopmentLogger("intermodeBaseModule"))
}

func mainWithArgs(ctx context.Context, args []string, logger golog.Logger) (err error) {
	modalModule, err := module.NewModuleFromArgs(ctx, logger)

	if err != nil {
		return err
	}
	modalModule.AddModelFromRegistry(ctx, base.Subtype, model)

	err = modalModule.Start(ctx)
	defer modalModule.Close(ctx)

	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// newBase creates a new base that underneath the hood sends canbus frames via
// a 10ms publishing loop.
func newBase(name string, logger golog.Logger) (base.Base, error) {
	socket, err := canbus.New()
	if err != nil {
		return nil, err
	}
	if err := socket.Bind(channel); err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	iBase := &interModeBase{
		name:          name,
		nextCommandCh: make(chan canbus.Frame),
		cancel:        cancel,
		logger:        logger,
	}

	iBase.activeBackgroundWorkers.Add(1)
	goutils.ManagedGo(func() {
		publishThread(cancelCtx, *socket, iBase.nextCommandCh, logger)
	}, iBase.activeBackgroundWorkers.Done)
	return iBase, nil
}

// constants from the data sheet.
const (
	channel        = "can0"
	driveId uint32 = 0x220
)

const (
	gearPark          = "park"
	gearReverse       = "reverse"
	gearNeutral       = "neutral"
	gearDrive         = "drive"
	gearEmergencyStop = "emergency_stop"

	steerModeFrontWheelDrive = "front-wheel-drive"
	steerModeRearWheelDrive  = "rear-wheel-drive"
	steerModeFourWheelDrive  = "four-wheel-drive"
	steerModeCrabSteering    = "crab-steering"
)

var (
	gears = map[string]byte{
		gearPark:          0,
		gearReverse:       1,
		gearNeutral:       2,
		gearDrive:         3,
		gearEmergencyStop: 4,
	}
	steerModes = map[string]byte{
		steerModeFrontWheelDrive: 0,
		steerModeRearWheelDrive:  1,
		steerModeFourWheelDrive:  2,
		steerModeCrabSteering:    3,
	}
)

type driveCommand struct {
	Accelerator   float64
	Brake         float64
	SteeringAngle float64
	Gear          byte
	SteerMode     byte
}

// calculateSteeringAngleBytes returns the intermode specific angle bytes for the given angle.
func calculateSteeringAngleBytes(angle float64) []byte {
	// angle from -90 to 90
	// positive is left, negative is right
	if math.Abs(angle) > 90 {
		if math.Signbit(angle) {
			angle = -90
		} else {
			angle = 90
		}
	}
	value := int16(angle / 0.0078125) // intermode scalar

	angleBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(angleBytes, uint16(value))
	return angleBytes
}

// calculateAccelAndBrakeBytes returns the intermode specific acceleration and brake bytes for the given
// acceleration percentage.
func calculateAccelAndBrakeBytes(accelPct float64) []byte {
	if accelPct == 0 {
		// 0 accel, 100 brake
		// where 100 is 1600 because of the steps, which i believe is 0x0640 in hex
		// but we flip our byte orders because the owner told us
		return []byte{0, 0, 0x40, 0x06} // LE
	}
	accelPct = math.Abs(accelPct)
	// nerf the speed to one fifth for safe office traversal
	accelPct /= 5
	value := uint16(accelPct / 0.0625) // intermode scalar

	acceloratorBytes := make([]byte, 4)
	binary.LittleEndian.PutUint16(acceloratorBytes[:2], value)
	return acceloratorBytes
}

type modalCommand interface {
	toFrame(logger golog.Logger) canbus.Frame
}

// toFrame convert the command to a canbus data frame.
func (cmd *driveCommand) toFrame(logger golog.Logger) canbus.Frame {
	frame := canbus.Frame{
		ID:   driveId,
		Data: make([]byte, 0, 8),
		Kind: canbus.SFF,
	}
	frame.Data = append(frame.Data, calculateAccelAndBrakeBytes(cmd.Accelerator)...)
	frame.Data = append(frame.Data, calculateSteeringAngleBytes(cmd.SteeringAngle)...)
	// is this the best place to be setting the gear to reverse? felt better than in each place that sets the forward motion.
	if cmd.Accelerator < 0 {
		cmd.Gear = gears[gearReverse]
	}
	frame.Data = append(frame.Data, cmd.Gear, cmd.SteerMode)

	logger.Debugw("frame", "data", frame.Data)

	return frame
}

type interModeBase struct {
	name                    string
	nextCommandCh           chan canbus.Frame
	activeBackgroundWorkers sync.WaitGroup
	cancel                  func()
	logger                  golog.Logger

	// generic.Unimplemented is a helper that embeds an unimplemented error in the Do method.
	generic.Unimplemented
}

// publishThread continuously sends the current command over the canbus.
func publishThread(
	ctx context.Context,
	socket canbus.Socket,
	nextCommandCh chan canbus.Frame,
	logger golog.Logger,
) {
	frame := (&stopCmd).toFrame(logger)

	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case frame = <-nextCommandCh:
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := socket.Send(frame); err != nil {
			logger.Errorw("send error", "error", err)
		}
	}
}

/*
	InterMode Base Implementation
	Every method will set the next command for the publish loop to send over the command bus forever.
*/

// MoveStraight moves the base forward the given distance and speed.
func (base *interModeBase) MoveStraight(ctx context.Context, distanceMm int, mmPerSec float64, extra map[string]interface{}) error {
	cmd := driveCommand{
		Accelerator:   50,
		Brake:         0,
		SteeringAngle: 0,
		Gear:          gears[gearDrive],
		SteerMode:     steerModes[steerModeFourWheelDrive],
	}

	if mmPerSec < 0 || distanceMm < 0 {
		cmd.Accelerator *= -1
	}

	if err := base.setNextCommand(ctx, &cmd); err != nil {
		return err
	}

	defer base.setNextCommand(ctx, &stopCmd)

	if !goutils.SelectContextOrWait(ctx, time.Duration(mmPerSec/float64(distanceMm))) {
		return ctx.Err()
	}

	return nil
}

// Spin spins the base by the given angleDeg and degsPerSec.
func (base *interModeBase) Spin(ctx context.Context, angleDeg, degsPerSec float64, extra map[string]interface{}) error {
	if err := base.setNextCommand(ctx, &driveCommand{
		Accelerator:   50,
		Brake:         0,
		SteeringAngle: angleDeg,
		Gear:          gears[gearDrive],
		SteerMode:     steerModes[steerModeFourWheelDrive],
	}); err != nil {
		return err
	}

	defer base.setNextCommand(ctx, &stopCmd)

	if !goutils.SelectContextOrWait(ctx, time.Duration(angleDeg/math.Abs(degsPerSec))) {
		return ctx.Err()
	}

	return nil
}

// SetPower sets the linear and angular [-1, 1] drive power.
func (base *interModeBase) SetPower(ctx context.Context, linear, angular r3.Vector, extra map[string]interface{}) error {
	return base.setNextCommand(ctx, &driveCommand{
		Accelerator:   linear.Y * 100,
		Brake:         0,
		SteeringAngle: angular.Z * 100,
		Gear:          gears[gearDrive],
		SteerMode:     steerModes[steerModeFourWheelDrive],
	})
}

// SetVelocity sets the linear (mmPerSec) and angular (degsPerSec) velocity.
func (base *interModeBase) SetVelocity(ctx context.Context, linear, angular r3.Vector, extra map[string]interface{}) error {
	return base.setNextCommand(ctx, &driveCommand{
		Accelerator:   linear.Y,
		Brake:         0,
		SteeringAngle: angular.Z * 100,
		Gear:          gears[gearDrive],
		SteerMode:     steerModes[steerModeFourWheelDrive],
	})
}

var stopCmd = driveCommand{
	Accelerator:   0,
	Brake:         100,
	SteeringAngle: 0,
	Gear:          gears[gearPark],
	SteerMode:     steerModes[steerModeFourWheelDrive],
}

// Stop stops the base. It is assumed the base stops immediately.
func (base *interModeBase) Stop(ctx context.Context, extra map[string]interface{}) error {
	return base.setNextCommand(ctx, &stopCmd)
}

func (base *interModeBase) IsMoving(ctx context.Context) (bool, error) {
	return false, utils.NewUnimplementedInterfaceError((*interModeBase)(nil), "intermodeBase does not yet support IsMoving()")
}

// DoCommand executes additional commands beyond the Base{} interface. For this rover that includes door open and close commands.
func (base *interModeBase) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	// TODO: expand this function to change steering/gearing modes.
	name, ok := cmd["command"]
	if !ok {
		return nil, errors.New("missing 'command' value")
	}
	switch name {

	default:
		return nil, fmt.Errorf("no such command: %s", name)
	}
}

// Close cleanly closes the base.
func (base *interModeBase) Close() {
	base.cancel()
	base.activeBackgroundWorkers.Wait()
}

func (base *interModeBase) setNextCommand(ctx context.Context, cmd modalCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case base.nextCommandCh <- cmd.toFrame(base.logger):
	}
	return nil
}
